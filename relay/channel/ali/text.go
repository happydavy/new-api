package ali

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"one-api/common"
	"one-api/dto"
	"one-api/relay/helper"
	"strings"

	"one-api/types"

	"github.com/gin-gonic/gin"
)

// https://help.aliyun.com/document_detail/613695.html?spm=a2c4g.2399480.0.0.1adb778fAdzP9w#341800c0f8w0r

const EnableSearchModelSuffix = "-internet"

func requestOpenAI2Ali(request dto.GeneralOpenAIRequest) *dto.GeneralOpenAIRequest {
	if request.TopP >= 1 {
		request.TopP = 0.999
	} else if request.TopP <= 0 {
		request.TopP = 0.001
	}
	return &request
}

func embeddingRequestOpenAI2Ali(request dto.EmbeddingRequest) *AliEmbeddingRequest {
	return &AliEmbeddingRequest{
		Model: request.Model,
		Input: struct {
			Texts []string `json:"texts"`
		}{
			Texts: request.ParseInput(),
		},
	}
}

func aliEmbeddingHandler(c *gin.Context, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	var fullTextResponse dto.FlexibleEmbeddingResponse
	err := json.NewDecoder(resp.Body).Decode(&fullTextResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}

	common.CloseResponseBodyGracefully(resp)

	model := c.GetString("model")
	if model == "" {
		model = "text-embedding-v4"
	}
	jsonResponse, err := json.Marshal(fullTextResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	c.Writer.Write(jsonResponse)
	return nil, &fullTextResponse.Usage
}

func embeddingResponseAli2OpenAI(response *AliEmbeddingResponse, model string) *dto.OpenAIEmbeddingResponse {
	openAIEmbeddingResponse := dto.OpenAIEmbeddingResponse{
		Object: "list",
		Data:   make([]dto.OpenAIEmbeddingResponseItem, 0, len(response.Output.Embeddings)),
		Model:  model,
		Usage:  dto.Usage{TotalTokens: response.Usage.TotalTokens},
	}

	for _, item := range response.Output.Embeddings {
		openAIEmbeddingResponse.Data = append(openAIEmbeddingResponse.Data, dto.OpenAIEmbeddingResponseItem{
			Object:    `embedding`,
			Index:     item.TextIndex,
			Embedding: item.Embedding,
		})
	}
	return &openAIEmbeddingResponse
}

func responseAli2OpenAI(response *AliResponse) *dto.OpenAITextResponse {
	choice := dto.OpenAITextResponseChoice{
		Index: 0,
		Message: dto.Message{
			Role:    "assistant",
			Content: response.Output.Text,
		},
		FinishReason: response.Output.FinishReason,
	}
	fullTextResponse := dto.OpenAITextResponse{
		Id:      response.RequestId,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Choices: []dto.OpenAITextResponseChoice{choice},
		Usage: dto.Usage{
			PromptTokens:     response.Usage.InputTokens,
			CompletionTokens: response.Usage.OutputTokens,
			TotalTokens:      response.Usage.InputTokens + response.Usage.OutputTokens,
		},
	}
	return &fullTextResponse
}

func streamResponseAli2OpenAI(aliResponse *AliResponse) *dto.ChatCompletionsStreamResponse {
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString(aliResponse.Output.Text)
	if aliResponse.Output.FinishReason != "null" {
		finishReason := aliResponse.Output.FinishReason
		choice.FinishReason = &finishReason
	}
	response := dto.ChatCompletionsStreamResponse{
		Id:      aliResponse.RequestId,
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "ernie-bot",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response
}

func aliStreamHandler(c *gin.Context, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	var usage dto.Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(bufio.ScanLines)
	dataChan := make(chan string)
	stopChan := make(chan bool)
	go func() {
		for scanner.Scan() {
			data := scanner.Text()
			if len(data) < 5 { // ignore blank line or wrong format
				continue
			}
			if data[:5] != "data:" {
				continue
			}
			data = data[5:]
			dataChan <- data
		}
		stopChan <- true
	}()
	helper.SetEventStreamHeaders(c)
	lastResponseText := ""
	c.Stream(func(w io.Writer) bool {
		select {
		case data := <-dataChan:
			var aliResponse AliResponse
			err := json.Unmarshal([]byte(data), &aliResponse)
			if err != nil {
				common.SysError("error unmarshalling stream response: " + err.Error())
				return true
			}
			if aliResponse.Usage.OutputTokens != 0 {
				usage.PromptTokens = aliResponse.Usage.InputTokens
				usage.CompletionTokens = aliResponse.Usage.OutputTokens
				usage.TotalTokens = aliResponse.Usage.InputTokens + aliResponse.Usage.OutputTokens
			}
			response := streamResponseAli2OpenAI(&aliResponse)
			response.Choices[0].Delta.SetContentString(strings.TrimPrefix(response.Choices[0].Delta.GetContentString(), lastResponseText))
			lastResponseText = aliResponse.Output.Text
			jsonResponse, err := json.Marshal(response)
			if err != nil {
				common.SysError("error marshalling stream response: " + err.Error())
				return true
			}
			c.Render(-1, common.CustomEvent{Data: "data: " + string(jsonResponse)})
			return true
		case <-stopChan:
			c.Render(-1, common.CustomEvent{Data: "data: [DONE]"})
			return false
		}
	})
	common.CloseResponseBodyGracefully(resp)
	return nil, &usage
}

func aliHandler(c *gin.Context, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	var aliResponse AliResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewError(err, types.ErrorCodeReadResponseBodyFailed), nil
	}
	common.CloseResponseBodyGracefully(resp)
	err = json.Unmarshal(responseBody, &aliResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	if aliResponse.Code != "" {
		return types.WithOpenAIError(types.OpenAIError{
			Message: aliResponse.Message,
			Type:    "ali_error",
			Param:   aliResponse.RequestId,
			Code:    aliResponse.Code,
		}, resp.StatusCode), nil
	}
	fullTextResponse := responseAli2OpenAI(&aliResponse)
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return nil, &fullTextResponse.Usage
}
