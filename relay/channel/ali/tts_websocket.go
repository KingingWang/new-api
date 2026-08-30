package ali

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const contextKeyAliTTSRequest = "ali_tts_request"

type aliWSRequestHeader struct {
	Action    string `json:"action"`
	TaskID    string `json:"task_id"`
	Streaming string `json:"streaming"`
}

type aliWSRequest struct {
	Header  aliWSRequestHeader `json:"header"`
	Payload any                `json:"payload"`
}

type aliWSStartPayload struct {
	Model      string         `json:"model"`
	TaskGroup  string         `json:"task_group"`
	Task       string         `json:"task"`
	Function   string         `json:"function"`
	Input      map[string]any `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

type aliWSContinueInput struct {
	Text string `json:"text"`
}

type aliWSContinuePayload struct {
	Model     string             `json:"model"`
	TaskGroup string             `json:"task_group"`
	Task      string             `json:"task"`
	Function  string             `json:"function"`
	Input     aliWSContinueInput `json:"input"`
}

type aliWSFinishPayload struct {
	Input map[string]any `json:"input"`
}

type aliWSEventHeader struct {
	TaskID       string `json:"task_id"`
	Event        string `json:"event"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

type aliWSEvent struct {
	Header  aliWSEventHeader `json:"header"`
	Payload struct {
		Output struct {
			Type string `json:"type"`
		} `json:"output"`
		Usage struct {
			Characters int `json:"characters"`
		} `json:"usage"`
	} `json:"payload"`
}

func isQwenAudioTTSModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(name, "qwen-audio-") && strings.Contains(name, "-tts-")
}

func aliQwenAudioTTSWebSocketURL(info *relaycommon.RelayInfo) string {
	if info != nil && strings.Contains(strings.ToLower(info.ChannelBaseUrl), "dashscope-intl.aliyuncs.com") {
		return "wss://dashscope-intl.aliyuncs.com/api-ws/v1/inference"
	}
	return "wss://dashscope.aliyuncs.com/api-ws/v1/inference"
}

func newAliWSRequest(action, taskID string, payload any) *aliWSRequest {
	return &aliWSRequest{
		Header: aliWSRequestHeader{
			Action:    action,
			TaskID:    taskID,
			Streaming: "duplex",
		},
		Payload: payload,
	}
}

func sendAliWSMessage(conn *websocket.Conn, message *aliWSRequest) error {
	data, err := common.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal ali websocket request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to write ali websocket message: %w", err)
	}
	return nil
}

// handleAliQwenAudioTTSResponse 调用 DashScope tts_v2 WebSocket 协议，
// 把 qwen-audio-3.0-tts-plus/flash 的二进制 WAV 帧透传/聚合给 OpenAI 客户端。
// 这两种模型不走 /api/v1/services/aigc/multimodal-generation/generation。
func handleAliQwenAudioTTSResponse(c *gin.Context, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	value, exists := c.Get(contextKeyAliTTSRequest)
	if !exists {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("ali TTS request not found in context"),
			types.ErrorCodeBadRequestBody,
			http.StatusInternalServerError,
		)
	}

	aliReq, ok := value.(*AliTTSRequest)
	if !ok {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("invalid ali TTS request type: %T", value),
			types.ErrorCodeBadRequestBody,
			http.StatusInternalServerError,
		)
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+info.ApiKey)

	conn, resp, dialErr := websocket.DefaultDialer.DialContext(c.Request.Context(), aliQwenAudioTTSWebSocketURL(info), header)
	if dialErr != nil {
		if resp != nil {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to connect ali TTS websocket: %w, status: %d", dialErr, resp.StatusCode),
				types.ErrorCodeBadResponseStatusCode,
				http.StatusBadGateway,
			)
		}
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to connect ali TTS websocket: %w", dialErr),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusBadGateway,
		)
	}
	defer conn.Close()

	taskID := strings.ReplaceAll(uuid.NewString(), "-", "")
	sampleRate := 48000
	rate := 1.0
	if aliReq.Input.Speed > 0 {
		rate = aliReq.Input.Speed
	}

	startPayload := aliWSStartPayload{
		Model:     aliReq.Model,
		TaskGroup: "audio",
		Task:      "tts",
		Function:  "SpeechSynthesizer",
		Input:     map[string]any{},
		Parameters: map[string]any{
			"voice":       aliReq.Input.Voice,
			"volume":      50,
			"text_type":   "PlainText",
			"sample_rate": sampleRate,
			"rate":        rate,
			"format":      "wav",
			"pitch":       1.0,
			"seed":        0,
			"type":        0,
		},
	}
	if err := sendAliWSMessage(conn, newAliWSRequest("run-task", taskID, startPayload)); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
	}

	continuePayload := aliWSContinuePayload{
		Model:     aliReq.Model,
		TaskGroup: "audio",
		Task:      "tts",
		Function:  "SpeechSynthesizer",
		Input:     aliWSContinueInput{Text: aliReq.Input.Text},
	}
	if err := sendAliWSMessage(conn, newAliWSRequest("continue-task", taskID, continuePayload)); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
	}

	finishPayload := aliWSFinishPayload{Input: map[string]any{}}
	if err := sendAliWSMessage(conn, newAliWSRequest("finish-task", taskID, finishPayload)); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
	}

	if info.IsStream {
		return streamAliQwenAudioTTS(c, conn, info)
	}
	return collectAliQwenAudioTTS(c, conn, info)
}

func parseAliWSEvent(data []byte) (aliWSEvent, error) {
	var event aliWSEvent
	if err := common.Unmarshal(data, &event); err != nil {
		return event, err
	}
	return event, nil
}

func collectAliQwenAudioTTS(c *gin.Context, conn *websocket.Conn, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	var audio bytes.Buffer
	totalCharacters := 0

	for {
		messageType, data, readErr := conn.ReadMessage()
		if readErr != nil {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to read ali TTS websocket message: %w", readErr),
				types.ErrorCodeReadResponseBodyFailed,
				http.StatusBadGateway,
			)
		}

		switch messageType {
		case websocket.TextMessage:
			event, parseErr := parseAliWSEvent(data)
			if parseErr != nil {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("failed to parse ali TTS websocket event: %w", parseErr),
					types.ErrorCodeBadResponseBody,
					http.StatusBadGateway,
				)
			}
			if event.Header.Event == "task-failed" {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("ali TTS task failed: %s %s", event.Header.ErrorCode, event.Header.ErrorMessage),
					types.ErrorCodeBadResponse,
					http.StatusBadRequest,
				)
			}
			if event.Payload.Usage.Characters > 0 {
				totalCharacters = event.Payload.Usage.Characters
			}
			if event.Header.Event == "task-finished" {
				goto done
			}
		case websocket.BinaryMessage:
			if _, writeErr := audio.Write(data); writeErr != nil {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("failed to buffer ali TTS audio: %w", writeErr),
					types.ErrorCodeDoRequestFailed,
					http.StatusInternalServerError,
				)
			}
		default:
			continue
		}
	}

done:
	if audio.Len() == 0 {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("ali TTS returned no audio data"),
			types.ErrorCodeBadResponse,
			http.StatusBadRequest,
		)
	}

	c.Data(http.StatusOK, "audio/wav", audio.Bytes())
	return aliQwenAudioTTSUsage(info, totalCharacters), nil
}

func streamAliQwenAudioTTS(c *gin.Context, conn *websocket.Conn, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	c.Writer.Header().Set("Content-Type", "audio/wav")
	c.Writer.WriteHeader(http.StatusOK)

	totalCharacters := 0
	for {
		messageType, data, readErr := conn.ReadMessage()
		if readErr != nil {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to read ali TTS websocket message: %w", readErr),
				types.ErrorCodeReadResponseBodyFailed,
				http.StatusBadGateway,
			)
		}

		switch messageType {
		case websocket.TextMessage:
			event, parseErr := parseAliWSEvent(data)
			if parseErr != nil {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("failed to parse ali TTS websocket event: %w", parseErr),
					types.ErrorCodeBadResponseBody,
					http.StatusBadGateway,
				)
			}
			if event.Header.Event == "task-failed" {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("ali TTS task failed: %s %s", event.Header.ErrorCode, event.Header.ErrorMessage),
					types.ErrorCodeBadResponse,
					http.StatusBadRequest,
				)
			}
			if event.Payload.Usage.Characters > 0 {
				totalCharacters = event.Payload.Usage.Characters
			}
			if event.Header.Event == "task-finished" {
				return aliQwenAudioTTSUsage(info, totalCharacters), nil
			}
		case websocket.BinaryMessage:
			if len(data) == 0 {
				continue
			}
			if _, writeErr := c.Writer.Write(data); writeErr != nil {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("failed to write ali TTS audio: %w", writeErr),
					types.ErrorCodeDoRequestFailed,
					http.StatusInternalServerError,
				)
			}
			c.Writer.Flush()
		default:
			continue
		}
	}
}

func aliQwenAudioTTSUsage(info *relaycommon.RelayInfo, totalCharacters int) *dto.Usage {
	return &dto.Usage{
		PromptTokens:     info.GetEstimatePromptTokens(),
		CompletionTokens: 0,
		TotalTokens:      totalCharacters,
	}
}
