package ali

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

type AliTTSRequest struct {
	Model string      `json:"model"`
	Input AliTTSInput `json:"input"`
}

type AliTTSInput struct {
	Text         string  `json:"text"`
	Voice        string  `json:"voice,omitempty"`
	LanguageType string  `json:"language_type,omitempty"`
	Speed        float64 `json:"speed,omitempty"`
	Volume       float64 `json:"volume,omitempty"`
	Pitch        float64 `json:"pitch,omitempty"`
}

type AliTTSResponse struct {
	Output    AliTTSOutput `json:"output"`
	Usage     AliTTSUsage  `json:"usage"`
	RequestID string       `json:"request_id"`
	Code      string       `json:"code,omitempty"`
	Message   string       `json:"message,omitempty"`
}

type AliTTSOutput struct {
	Audio AliTTSAudio `json:"audio,omitempty"`
}

type AliTTSAudio struct {
	URL       string `json:"url,omitempty"`
	Data      string `json:"data,omitempty"`
	ID        string `json:"id,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

type AliTTSUsage struct {
	Characters int `json:"characters"`
}

var openAIToAliVoiceMap = map[string]string{
	"alloy":   "Cherry",
	"echo":    "Alex",
	"fable":   "Bella",
	"onyx":    "Olivia",
	"nova":    "Luna",
	"shimmer": "Emily",
}

func mapOpenAIVoiceToAli(openAIVoice string) string {
	if voice, ok := openAIToAliVoiceMap[openAIVoice]; ok {
		return voice
	}
	return openAIVoice
}

func convertOpenAITTSRequestToAli(oaiReq dto.AudioRequest) *AliTTSRequest {
	aliReq := &AliTTSRequest{
		Model: oaiReq.Model,
		Input: AliTTSInput{
			Text:         oaiReq.Input,
			Voice:        mapOpenAIVoiceToAli(oaiReq.Voice),
			LanguageType: "Chinese",
		},
	}

	if oaiReq.Speed != nil && *oaiReq.Speed > 0 {
		aliReq.Input.Speed = *oaiReq.Speed
	}

	return aliReq
}

func handleAliTTSResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to read ali TTS response: %w", readErr),
			types.ErrorCodeReadResponseBodyFailed,
			http.StatusInternalServerError,
		)
	}
	defer resp.Body.Close()

	var aliResp AliTTSResponse
	if unmarshalErr := common.Unmarshal(body, &aliResp); unmarshalErr != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to unmarshal ali TTS response: %w", unmarshalErr),
			types.ErrorCodeBadResponseBody,
			http.StatusInternalServerError,
		)
	}

	if aliResp.Code != "" && aliResp.Code != "Success" {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("ali TTS error: %s - %s", aliResp.Code, aliResp.Message),
			types.ErrorCodeBadResponse,
			http.StatusBadRequest,
		)
	}

	// 优先使用 URL：服务端下载后直接回吐音频字节，而不是 302 重定向，
	// 因为多数 OpenAI 兼容客户端默认不跟随重定向。
	if aliResp.Output.Audio.URL != "" {
		audioData, contentType, fetchErr := fetchAliAudioURL(c, aliResp.Output.Audio.URL)
		if fetchErr != nil {
			return nil, types.NewErrorWithStatusCode(
				fetchErr,
				types.ErrorCodeDoRequestFailed,
				http.StatusInternalServerError,
			)
		}
		c.Data(http.StatusOK, contentType, audioData)
	} else if aliResp.Output.Audio.Data != "" {
		// 如果是 base64 编码的音频数据
		audioData, decodeErr := base64.StdEncoding.DecodeString(aliResp.Output.Audio.Data)
		if decodeErr != nil {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to decode audio data: %w", decodeErr),
				types.ErrorCodeBadResponse,
				http.StatusInternalServerError,
			)
		}
		c.Data(http.StatusOK, "audio/mpeg", audioData)
	} else {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("no audio URL or data in ali TTS response"),
			types.ErrorCodeBadResponse,
			http.StatusBadRequest,
		)
	}

	usage = &dto.Usage{
		PromptTokens:     info.GetEstimatePromptTokens(),
		CompletionTokens: 0,
		TotalTokens:      aliResp.Usage.Characters,
	}

	return usage, nil
}

func fetchAliAudioURL(c *gin.Context, rawURL string) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid audio url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("unsupported audio url scheme: %s", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to build audio download request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to download audio: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("audio url returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read audio data: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/wav"
	}
	return data, contentType, nil
}
