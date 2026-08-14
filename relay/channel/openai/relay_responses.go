package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// inspectResponsesOutput reports whether the Responses output items carry
// usable output (non-empty message content or any tool call item) and
// accumulates the output text for validation. Reasoning-only output counts as
// empty.
func inspectResponsesOutput(output []dto.ResponsesOutput) (string, bool) {
	var text strings.Builder
	hasOutput := false
	for i := range output {
		item := &output[i]
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				text.WriteString(content.Text)
				if strings.TrimSpace(content.Text) != "" {
					hasOutput = true
				}
			}
		case "reasoning":
			// reasoning-only output is treated as empty
		default:
			// function calls, web search, image generation, etc. are usable
			// output; unknown item types are conservatively treated as usable
			hasOutput = true
		}
	}
	return text.String(), hasOutput
}

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(responsesResponse.Model)
	responseBody = rewriteSGLangResponsesCreatedAt(info, responseBody, "created_at", responsesResponse.CreatedAt)

	// Validate model output before anything is written to the client: retry
	// empty/thinking-only responses and outputs matching the blacklist.
	if helper.ResponseValidationActive() {
		text, hasOutput := inspectResponsesOutput(responsesResponse.Output)
		if apiErr := helper.CheckModelOutput(c, text, hasOutput); apiErr != nil {
			return nil, apiErr
		}
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := &dto.Usage{}
	service.ApplyResponsesUsage(usage, responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			idx := i
			imageCounter.Observe(&responsesResponse.Output[i], &idx)
		}
	}
	imageCounter.Commit(info)

	return usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	accumulator := service.NewResponsesUsageAccumulator(info)
	var responseTextBuilder strings.Builder
	var hasText bool
	var hasOutputItem bool

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}
		if streamResponse.Response != nil {
			data = string(rewriteSGLangResponsesCreatedAt(info, []byte(data), "response.created_at", streamResponse.Response.CreatedAt))
		}

		switch streamResponse.Type {
		case "response.completed", "response.done":
			if streamResponse.Response != nil {
				completedText, hasCompletedOutput := inspectResponsesOutput(streamResponse.Response.Output)
				if hasCompletedOutput {
					hasOutputItem = true
				}
				if completedText != "" && !hasText {
					responseTextBuilder.WriteString(completedText)
				}
			}
		case "response.output_text.delta":
			// 处理输出文本
			responseTextBuilder.WriteString(streamResponse.Delta)
			if strings.TrimSpace(streamResponse.Delta) != "" {
				hasText = true
			}
		case dto.ResponsesOutputTypeItemDone:
			if streamResponse.Item != nil && streamResponse.Item.Type != "message" && streamResponse.Item.Type != "reasoning" {
				hasOutputItem = true
			}
		}
		sendResponsesStreamData(c, streamResponse, data)
		accumulator.Observe(&streamResponse)
	})

	// Validate model output while no response bytes have been written to the
	// client: retry empty/thinking-only responses and
	// outputs matching the blacklist. Once stream data has been forwarded, the
	// response cannot be replayed on another channel.
	if helper.ResponseValidationActive() {
		if apiErr := helper.CheckModelOutput(c, responseTextBuilder.String(), hasText || hasOutputItem); apiErr != nil {
			if helper.StreamResponseRetryAvailable(c) {
				helper.ResetEventStreamHeaders(c)
				return nil, apiErr
			}
			logger.LogError(c, fmt.Sprintf("invalid upstream response detected, but stream data was already sent, skip retry: %s", apiErr.Error()))
		}
	}

	common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, info.StreamStatus)
	info.StreamStatus.RequireTerminal()
	return accumulator.Finish(), nil
}

func rewriteSGLangResponsesCreatedAt(info *relaycommon.RelayInfo, payload []byte, path string, createdAt dto.IntValue) []byte {
	if info.GetChannelType() != constant.ChannelTypeSGLang {
		return payload
	}
	if !gjson.GetBytes(payload, path).Exists() {
		return payload
	}
	patched, err := sjson.SetBytes(payload, path, int(createdAt))
	if err != nil {
		return payload
	}
	return patched
}
