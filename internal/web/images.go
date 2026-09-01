package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	designerAppServiceScope  = "https://designerappservice.officeapps.live.com/.default"
	maxGeneratedImageBytes   = 20 << 20
	maxImageEditRequestBytes = maxGeneratedImageBytes + (2 << 20)
	generatedImageTTL        = 15 * time.Minute
	maxGeneratedImages       = 128
	// imageAccountAttempts bounds how many accounts one image request may try
	// before the throttle is reported to the caller.
	imageAccountAttempts = 3
)

// errImageQuotaRefused marks an upstream 200 whose text is a natural-language
// refusal about the image quota, so the failover loop can treat it like a
// throttle without turning it into an upstream error.
var errImageQuotaRefused = errors.New("upstream refused image generation: quota exhausted")

// errImageServiceUnavailable marks an upstream 200 that reports the image
// service itself as unavailable or overloaded — transient, and worth trying on
// another account before giving up.
var errImageServiceUnavailable = errors.New("upstream image generation service is unavailable")

// imageFailoverWorthwhile reports whether another account has a chance of
// succeeding. Only quota and throttling errors qualify; a content-policy block
// or a malformed request would fail the same way everywhere.
func imageFailoverWorthwhile(err error) bool {
	return IsRateLimited(err) || errors.Is(err, errImageQuotaRefused) || errors.Is(err, errImageServiceUnavailable) || errors.Is(err, chathub.ErrImageLimit)
}

// isImageCapabilityThrottle reports whether the error is an image-metering
// throttle, i.e. one that says nothing about the account's chat capacity.
func isImageCapabilityThrottle(err error) bool {
	return errors.Is(err, chathub.ErrImageLimit) || errors.Is(err, chathub.ErrMeteringThrottled)
}

// markImageThrottle applies an image-only cooldown so rotation skips this
// account for image generation while leaving its chat capacity alone. The daily
// token quota lasts until tomorrow; a system-capacity throttle is transient.
func (s *Server) markImageThrottle(accountID string, err error) {
	if s == nil || s.accountPool == nil || accountID == "" {
		return
	}
	switch {
	case errors.Is(err, chathub.ErrImageLimit):
		s.accountPool.MarkImageGenTokensThrottled(accountID)
	case errors.Is(err, chathub.ErrMeteringThrottled):
		s.accountPool.MarkImageGenSystemThrottled(accountID)
	}
}

// imageURLsFromResult collects the generated image URLs, falling back to the raw
// result and answer text when the upstream omits the structured field.
func imageURLsFromResult(res chathub.Result) []string {
	if len(res.Images) > 0 {
		return res.Images
	}
	if urls := extractImageURLs(res.RawResult); len(urls) > 0 {
		return urls
	}
	return extractImageURLs(res.Text)
}

func logImageGenDebug(res chathub.Result) {
	textPreview := res.Text
	if len(textPreview) > 500 {
		textPreview = textPreview[:500]
	}
	rawPreview := res.RawResult
	if len(rawPreview) > 500 {
		rawPreview = rawPreview[:500]
	}
	debug := map[string]any{"text": textPreview, "raw_len": len(res.RawResult), "events": len(res.Events), "images": res.Images, "raw_preview": rawPreview}
	encoded, _ := json.Marshal(debug)
	log.Printf("[image-gen-debug] %s", string(encoded))
}

type generatedImage struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time
}

type imageGenerationRequest struct {
	Prompt         string               `json:"prompt"`
	N              int                  `json:"n"`
	Size           string               `json:"size"`
	ResponseFormat string               `json:"response_format"`
	Model          string               `json:"model"`
	AccountID      string               `json:"accountId"`
	User           string               `json:"user"`
	Operation      string               `json:"operation,omitempty"`
	Attachments    []chathub.Attachment `json:"attachments,omitempty"`
}

func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var b imageGenerationRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxImageEditRequestBytes)
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	if b.N <= 0 {
		b.N = 1
	}
	if b.N > 10 {
		writeOpenAIError(w, 400, "invalid_request_error", "n must be between 1 and 10")
		return
	}
	format := strings.ToLower(strings.TrimSpace(b.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "response_format must be url or b64_json")
		return
	}
	acc, err := s.resolveImageAccount(firstNonEmpty(b.AccountID, b.User))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "account missing oid/tid — re-login with PKCE")
		return
	}
	imageTimeout := time.Duration(s.settings.get().ImageTimeoutSeconds) * time.Second
	size := b.Size
	if size == "" {
		size = "1024x1024"
	}
	endpoint := "/v1/images/generations"
	prompt := fmt.Sprintf("Generate an image with GPT Image 2. Size: %s. Description: %s. Return the image URL directly.", size, b.Prompt)
	if b.Operation == "edit" {
		if len(b.Attachments) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image is required")
			return
		}
		endpoint = "/v1/images/edits"
		prompt = fmt.Sprintf("Edit the first attached image with GPT Image 2. Size: %s. Instructions: %s. Preserve everything not requested to change. Return the edited image URL directly.", size, b.Prompt)
	}

	// A throttled account must not become the caller's 429: rotate onto another
	// account the way the chat path does, and only report the throttle once no
	// candidate is left.
	var (
		res     chathub.Result
		images  []string
		lastErr error
	)
	pinned := firstNonEmpty(b.AccountID, b.User) != ""
	tried := map[string]bool{}
	for attempt := 1; ; attempt++ {
		tried[acc.ID] = true
		attemptCtx, cancel := context.WithTimeout(r.Context(), imageTimeout)
		res, lastErr = s.chatWithAccount(attemptCtx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{Text: prompt, Tone: "magic", Attachments: b.Attachments, LicenseType: s.settings.get().LicenseType, Scenario: s.settings.get().Scenario, FeatureFlags: s.featureFlags(), Capability: "ImageGeneration"})
		cancel()
		if lastErr == nil {
			log.Printf("[image-gen] conversation=%s images=%d text_len=%d events=%d raw_len=%d", res.ConversationID, len(res.Images), len(res.Text), len(res.Events), len(res.RawResult))
			images = imageURLsFromResult(res)
			if len(images) > 0 {
				break
			}
			switch classifyImageRefusal(strings.Join([]string{res.Text, res.RawResult}, "\n")) {
			case imageRefusalQuota:
				// Upstream answered 200 with a natural-language refusal, so
				// chatWithAccount already recorded a success and cleared this
				// account's cooldown. Mark the image quota now: the write lands
				// after that MarkSuccess, and later ones preserve it.
				s.accountPool.MarkImageGenTokensThrottled(acc.ID)
				lastErr = errImageQuotaRefused
			case imageRefusalCapacity:
				// The upstream image service itself is down or overloaded rather
				// than this account being out of quota, so hold the account back
				// briefly and let another one try.
				s.accountPool.MarkImageGenSystemThrottled(acc.ID)
				lastErr = errImageServiceUnavailable
			case imageRefusalPolicy:
				// Every account would refuse the same prompt; report it as the
				// client's problem so it does not retry and burn more quota.
				logImageGenDebug(res)
				writeOpenAIError(w, http.StatusBadRequest, "content_policy_violation", firstNonEmpty(strings.TrimSpace(res.Text), "upstream refused to generate this image"))
				return
			default:
				logImageGenDebug(res)
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned no image resource")
				return
			}
		} else {
			s.markImageThrottle(acc.ID, lastErr)
		}
		if pinned || attempt >= imageAccountAttempts || !imageFailoverWorthwhile(lastErr) {
			break
		}
		next, nerr := s.nextImageAccount(tried)
		if nerr != nil {
			break
		}
		if next.OID == "" || next.TID == "" {
			next.OID, next.TID = extractOIDTID(next.AccessToken)
		}
		if next.OID == "" || next.TID == "" {
			break
		}
		log.Printf("[image-gen] account=%s throttled (%v), failing over to account=%s", acc.ID, lastErr, next.ID)
		acc = next
	}
	if lastErr != nil {
		switch {
		case errors.Is(lastErr, errImageQuotaRefused):
			w.Header().Set("Retry-After", strconv.Itoa(s.imageRetryAfter(acc.ID)))
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "M365 image generation quota is exhausted; try again later or use another account")
		case errors.Is(lastErr, errImageServiceUnavailable):
			w.Header().Set("Retry-After", strconv.Itoa(s.imageRetryAfter(acc.ID)))
			writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_unavailable", "M365 image generation is temporarily unavailable upstream; retry shortly")
		default:
			log.Printf("[image-gen] endpoint=%s account=%s failed: %v", endpoint, acc.ID, lastErr)
			writeUpstreamError(w, lastErr)
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), imageTimeout)
	defer cancel()
	if len(images) > b.N {
		images = images[:b.N]
	}

	var designerToken string
	data := make([]map[string]string, 0, len(images))
	for _, sourceURL := range images {
		if strings.HasPrefix(strings.ToLower(sourceURL), "data:image/") {
			if format == "b64_json" {
				parts := strings.SplitN(sourceURL, ",", 2)
				if len(parts) != 2 {
					writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "invalid upstream image data")
					return
				}
				data = append(data, map[string]string{"b64_json": parts[1]})
			} else {
				data = append(data, map[string]string{"url": sourceURL})
			}
			continue
		}
		if !isDesignerImageURL(sourceURL) {
			if format == "b64_json" {
				writeOpenAIError(w, http.StatusBadGateway, "unsupported_response_format", "upstream returned URL, not b64_json")
				return
			}
			data = append(data, map[string]string{"url": sourceURL})
			continue
		}
		if designerToken == "" {
			designerToken, err = s.designerAccessToken(acc)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", upstreamError(err))
				return
			}
		}
		imageData, contentType, err := downloadDesignerImage(ctx, sourceURL, designerToken)
		if err != nil {
			log.Printf("[image-gen-download] err=%v", err)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", upstreamError(err))
			return
		}
		if format == "b64_json" {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(imageData)})
			continue
		}
		id := s.storeGeneratedImage(imageData, contentType)
		data = append(data, map[string]string{"url": generatedImageURL(r, id)})
	}

	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		ClientIP:     clientIP(r),
		AccountEmail: acc.Email,
		Model:        firstNonEmpty(b.Model, "gpt-image-2"),
		Endpoint:     endpoint,
		InputTokens:  EstimateTokens(prompt),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	jsonOut(w, map[string]any{"created": time.Now().Unix(), "data": data, "m365": map[string]any{"conversationId": res.ConversationID, "sessionId": res.SessionID, "images": images}})
}

func (s *Server) imageEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageEditRequestBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid multipart image edit request")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		file, header, err = r.FormFile("image[]")
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image is required")
		return
	}
	defer file.Close()
	imageData, err := io.ReadAll(io.LimitReader(file, maxGeneratedImageBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not read image")
		return
	}
	if len(imageData) > maxGeneratedImageBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "image exceeds 20 MiB")
		return
	}
	contentType := http.DetectContentType(imageData)
	ext := ""
	switch contentType {
	case "image/png":
		ext = "png"
	case "image/jpeg":
		ext = "jpg"
	case "image/webp":
		ext = "webp"
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image must be PNG, JPEG, or WebP")
		return
	}
	name := strings.TrimSpace(header.Filename)
	if name == "" {
		name = "image." + ext
	}
	n := 1
	if rawN := strings.TrimSpace(r.FormValue("n")); rawN != "" {
		n, err = strconv.Atoi(rawN)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "n must be an integer")
			return
		}
	}
	body := imageGenerationRequest{
		Prompt:         prompt,
		N:              n,
		Size:           strings.TrimSpace(r.FormValue("size")),
		ResponseFormat: strings.TrimSpace(r.FormValue("response_format")),
		Model:          strings.TrimSpace(r.FormValue("model")),
		AccountID:      firstNonEmpty(strings.TrimSpace(r.FormValue("accountId")), strings.TrimSpace(r.FormValue("account_id"))),
		User:           strings.TrimSpace(r.FormValue("user")),
		Operation:      "edit",
		Attachments: []chathub.Attachment{{
			Type:     "image",
			URL:      "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(imageData),
			Name:     name,
			MimeType: contentType,
		}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "could not encode image edit request")
		return
	}
	next := r.Clone(r.Context())
	next.Body = io.NopCloser(bytes.NewReader(encoded))
	next.ContentLength = int64(len(encoded))
	next.Header = r.Header.Clone()
	next.Header.Set("Content-Type", "application/json")
	next.Form = nil
	next.PostForm = nil
	next.MultipartForm = nil
	s.imageGenerations(w, next)
}

func (s *Server) designerAccessToken(acc auth.AccountToken) (string, error) {
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return "", fmt.Errorf("account has no refresh token for Designer image download")
	}
	clientID := firstNonEmpty(acc.ClientID, auth.ClientID())
	set, err := auth.RefreshWithScope(acc.RefreshToken, clientID, designerAppServiceScope)
	if err != nil {
		return "", fmt.Errorf("obtain Designer image token: %w", err)
	}
	if set.RefreshToken != "" && set.RefreshToken != acc.RefreshToken {
		if err := s.tokens.UpdateRefreshToken(acc.ID, set.RefreshToken); err != nil {
			log.Printf("[image-gen] rotated refresh token could not be persisted account=%s err=%v", acc.ID, err)
		}
	}
	return set.AccessToken, nil
}

func isDesignerImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "designerapp.officeapps.live.com")
}

func downloadDesignerImage(ctx context.Context, rawURL, accessToken string) ([]byte, string, error) {
	if !isDesignerImageURL(rawURL) {
		return nil, "", fmt.Errorf("unsupported generated image host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "image/*")
	client := *outbound.HTTPClient()
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !isDesignerImageURL(next.URL.String()) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Designer image download HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxGeneratedImageBytes {
		return nil, "", fmt.Errorf("generated image exceeds %d bytes", maxGeneratedImageBytes)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = http.DetectContentType(body)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return nil, "", fmt.Errorf("Designer returned non-image content")
	}
	return body, contentType, nil
}

func (s *Server) storeGeneratedImage(data []byte, contentType string) string {
	id := uuid.NewString()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generatedImages == nil {
		s.generatedImages = map[string]generatedImage{}
	}
	for key, item := range s.generatedImages {
		if now.After(item.ExpiresAt) {
			delete(s.generatedImages, key)
		}
	}
	if len(s.generatedImages) >= maxGeneratedImages {
		var oldestID string
		var oldest time.Time
		for key, item := range s.generatedImages {
			if oldestID == "" || item.ExpiresAt.Before(oldest) {
				oldestID, oldest = key, item.ExpiresAt
			}
		}
		if oldestID != "" {
			delete(s.generatedImages, oldestID)
		}
	}
	s.generatedImages[id] = generatedImage{Data: append([]byte(nil), data...), ContentType: contentType, ExpiresAt: now.Add(generatedImageTTL)}
	return id
}

func generatedImageURL(r *http.Request, id string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/v1/images/files/%s", scheme, r.Host, id)
}

func (s *Server) generatedImageFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/images/files/")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	s.mu.Lock()
	item, ok := s.generatedImages[id]
	if ok && now.After(item.ExpiresAt) {
		delete(s.generatedImages, id)
		ok = false
	}
	if ok {
		item.Data = append([]byte(nil), item.Data...)
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Length", fmt.Sprint(len(item.Data)))
	_, _ = w.Write(item.Data)
}

func isImageQuotaRefusal(text string) bool {
	return classifyImageRefusal(text) == imageRefusalQuota
}

// imageRefusalKind tells apart the reasons an upstream 200 can carry no image.
// The distinction decides both the status code the caller sees and whether
// another account is worth trying.
type imageRefusalKind int

const (
	// imageRefusalUnknown covers a missing image with no recognizable reason.
	imageRefusalUnknown imageRefusalKind = iota
	// imageRefusalQuota is this account's image allowance being spent.
	imageRefusalQuota
	// imageRefusalCapacity is a transient upstream outage or overload.
	imageRefusalCapacity
	// imageRefusalPolicy is a content decision that every account would repeat.
	imageRefusalPolicy
)

func classifyImageRefusal(text string) imageRefusalKind {
	low := strings.ToLower(strings.TrimSpace(text))
	if low == "" {
		return imageRefusalUnknown
	}
	for _, phrase := range []string{
		"generate any more images",
		"image generation quota",
		"daily image limit",
		"try again tomorrow",
		"无法再生成图片",
		"请明天再试",
	} {
		if strings.Contains(low, phrase) {
			return imageRefusalQuota
		}
	}
	for _, phrase := range []string{
		"image generation service is currently unavailable",
		"image generation service is currently experiencing",
		"image generation is temporarily unavailable",
		"图片生成服务暂时不可用",
	} {
		if strings.Contains(low, phrase) {
			return imageRefusalCapacity
		}
	}
	for _, phrase := range []string{
		"can’t generate that image",
		"can't generate that image",
		"can’t generate images",
		"can't generate images",
		"copyrighted character",
		"against our content policy",
		"违反内容政策",
	} {
		if strings.Contains(low, phrase) {
			return imageRefusalPolicy
		}
	}
	return imageRefusalUnknown
}

// extractImageURLs finds image URLs in a raw JSON string by searching for URL patterns.
func extractImageURLs(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				lk := strings.ToLower(k)
				if s, ok := e.(string); ok && (lk == "url" || lk == "imageurl" || lk == "thumbnailurl" || lk == "downloadurl" || lk == "src" || lk == "value" || lk == "data") {
					if strings.HasPrefix(s, "https://") && !seen[s] {
						if strings.Contains(strings.ToLower(s), "image") || strings.HasSuffix(strings.ToLower(s), ".png") || strings.HasSuffix(strings.ToLower(s), ".jpg") || strings.HasSuffix(strings.ToLower(s), ".jpeg") || strings.HasSuffix(strings.ToLower(s), ".webp") || strings.HasSuffix(strings.ToLower(s), ".gif") {
							seen[s] = true
							out = append(out, s)
						}
					}
				} else {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return out
}

func downloadImageAsBase64(url string) (b64, contentType string, err error) {
	return downloadImageAsBase64WithToken(url, "")
}

func downloadImageAsBase64WithToken(url, token string) (b64, contentType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("download returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return "", "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	enc := base64.StdEncoding.EncodeToString(body)
	return enc, ct, nil
}

func downloadImageAsDataURI(url string) (string, error) {
	b64, ct, err := downloadImageAsBase64(url)
	if err != nil {
		return url, nil
	}
	return "data:" + ct + ";base64," + b64, nil
}

func downloadImageAsDataURIWithToken(url, token string) (string, error) {
	b64, ct, err := downloadImageAsBase64WithToken(url, token)
	if err != nil {
		urlPreview := url
		if len(urlPreview) > 80 {
			urlPreview = urlPreview[:80]
		}
		log.Printf("[image-download] failed url=%s token_len=%d err=%v", urlPreview, len(token), err)
		return url, nil
	}
	urlPreview := url
	if len(urlPreview) > 80 {
		urlPreview = urlPreview[:80]
	}
	log.Printf("[image-download] ok url=%s ct=%s size=%d", urlPreview, ct, len(b64))
	return "data:" + ct + ";base64," + b64, nil
}
