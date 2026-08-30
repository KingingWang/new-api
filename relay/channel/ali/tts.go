package ali

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
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

func mapOpenAIVoiceToAli(model, openAIVoice string) string {
	// qwen-audio-3.0-tts 走 tts_v2 WebSocket，系统音色和 qwen3-tts
	// 不是同一套。OpenAI 客户端通常只传 alloy/echo/...，需要映射到
	// 当前模型真实支持的音色，避免 InvalidParameter（voice 不支持）。
	if isQwenAudioTTSModel(model) {
		voice := strings.ToLower(strings.TrimSpace(openAIVoice))
		defaultVoice := "longanlingxin"
		mapping := map[string]string{
			"alloy":   "longanlingxin",
			"echo":    "longanlufeng",
			"fable":   "longanlingxin",
			"onyx":    "longanlufeng",
			"nova":    "longanlingxin",
			"shimmer": "longanlingxin",
		}
		if strings.Contains(strings.ToLower(model), "-tts-flash") {
			defaultVoice = "longanlingxi"
			mapping = map[string]string{
				"alloy":   "longanlingxi",
				"echo":    "longanxiaoxin",
				"fable":   "longanfengyue",
				"onyx":    "longchuanshu_v3.6",
				"nova":    "longanlingxi",
				"shimmer": "longanyuanfei",
			}
		}
		if mapped, ok := mapping[voice]; ok {
			return mapped
		}
		if voice == "" {
			return defaultVoice
		}
		// 保留用户直接传入的音色 ID（例如声音复刻音色）。
		return openAIVoice
	}

	if voice, ok := openAIToAliVoiceMap[openAIVoice]; ok {
		return voice
	}
	return openAIVoice
}

func convertOpenAITTSRequestToAli(oaiReq dto.AudioRequest, model string) *AliTTSRequest {
	languageType := "Chinese"
	if isQwenAudioTTSModel(model) {
		languageType = ""
	}

	aliReq := &AliTTSRequest{
		Model: model,
		Input: AliTTSInput{
			Text:         oaiReq.Input,
			Voice:        mapOpenAIVoiceToAli(model, oaiReq.Voice),
			LanguageType: languageType,
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

// handleAliTTSStreamResponse 将 DashScope 的 SSE 流式响应转码为 WAV 字节流返回给客户端。
//
// DashScope 侧通过 X-DashScope-SSE 开启流式，返回的每一帧 data 是 JSON：
//
//	{"output":{"audio":{"data":"<base64 16bit PCM>"}}}
//
// 而多数 OpenAI 兼容客户端（例如小智的 openai provider）读取的是原始音频字节流，
// 因此这里把 SSE 帧解码成 PCM，并在首帧补一个 WAV 头，按字节流下发给客户端。
func handleAliTTSStreamResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	u := &dto.Usage{
		PromptTokens: info.GetEstimatePromptTokens(),
	}
	defer resp.Body.Close()

	c.Writer.Header().Set("Content-Type", "audio/wav")
	c.Writer.WriteHeader(http.StatusOK)

	scanner := helper.NewStreamScanner(resp.Body)
	wroteHeader := false

	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 6 || line[:5] != "data:" {
			continue
		}

		payload := strings.TrimSpace(line[5:])
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var ev AliTTSResponse
		if unmarshalErr := common.Unmarshal([]byte(payload), &ev); unmarshalErr == nil && ev.Usage.Characters > 0 {
			u.TotalTokens = ev.Usage.Characters
		}
		if ev.Output.Audio.Data == "" {
			continue
		}

		pcm, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(ev.Output.Audio.Data))
		if decodeErr != nil {
			return u, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to decode ali TTS audio data: %w", decodeErr),
				types.ErrorCodeBadResponseBody,
				http.StatusInternalServerError,
			)
		}
		if len(pcm) == 0 {
			continue
		}

		if !wroteHeader {
			// DashScope 有时首帧自带完整 WAV 头，有时只有裸 PCM。
			// 已经带 WAV 头就直接透传，否则我们自己补一个。
			if !isRIFFWave(pcm) {
				sampleRate := 24000
				if strings.Contains(strings.ToLower(info.UpstreamModelName), "qwen-audio") {
					sampleRate = 48000
				}
				if _, hdrErr := c.Writer.Write(buildWAVHeader(sampleRate, 1, 16)); hdrErr != nil {
					return u, types.NewErrorWithStatusCode(
						fmt.Errorf("failed to write WAV header: %w", hdrErr),
						types.ErrorCodeDoRequestFailed,
						http.StatusInternalServerError,
					)
				}
			}
			wroteHeader = true
		}

		if _, writeErr := c.Writer.Write(pcm); writeErr != nil {
			return u, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to write audio data: %w", writeErr),
				types.ErrorCodeDoRequestFailed,
				http.StatusInternalServerError,
			)
		}
		c.Writer.Flush()
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return u, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to read ali TTS stream: %w", scanErr),
			types.ErrorCodeReadResponseBodyFailed,
			http.StatusInternalServerError,
		)
	}

	return u, nil
}

// isRIFFWave 判断一段字节是否是带 RIFF/WAVE 头的 WAV 数据至少 12 字节的前缀。
func isRIFFWave(b []byte) bool {
	return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE"
}

// buildWAVHeader 构造一个 44 字节的标准 PCM WAV 头。流式场景下 RIFF/data
// 长度无法预先知道，填 0 即可；客户端只关心采样率与声道数。
func buildWAVHeader(sampleRate, channels, bitsPerSample int) []byte {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], 0)
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:36], uint16(bitsPerSample))
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], 0)
	return h
}
