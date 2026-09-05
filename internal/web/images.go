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
	"regexp"
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
	// imageRequestBudgetFactor bounds the whole request relative to the
	// per-attempt image timeout, leaving room for roughly one failover plus the
	// download phase instead of granting every phase a fresh full timeout.
	imageRequestBudgetFactor = 2
	// imageDownloadBudget bounds the Designer download phase. The overall request
	// deadline clamps it as well, so it can only ever shorten the wait.
	imageDownloadBudget = 60 * time.Second
	// designerDownloadAttempts bounds the retries for one generated image. The
	// upstream hands back a short-lived URL, so a transient 5xx or reset
	// connection is worth another look, but a long loop would outlive the URL.
	designerDownloadAttempts = 3
	designerRetryBackoff     = 400 * time.Millisecond
)

// errImageQuotaRefused marks an upstream 200 whose text is a natural-language
// refusal about the image quota, so the failover loop can treat it like a
// throttle without turning it into an upstream error.
var errImageQuotaRefused = errors.New("upstream refused image generation: quota exhausted")

// errImageServiceUnavailable marks an upstream 200 that reports the image
// service itself as unavailable or overloaded — transient, and worth trying on
// another account before giving up.
var errImageServiceUnavailable = errors.New("upstream image generation service is unavailable")

// errImageNoResource marks an upstream 200 that carried neither an image nor a
// recognizable refusal. It used to fail the request on the spot, which made a
// transient upstream miss indistinguishable from a permanent one; it is the same
// class of miss as an empty completion and deserves the same second chance.
var errImageNoResource = errors.New("upstream returned no image resource")

// imageDeliveryFailure records why one generated image could not be handed to
// the caller. Only the first one is reported, and only when no image at all
// could be delivered.
type imageDeliveryFailure struct {
	status  int
	code    string
	message string
}

// imageFailoverWorthwhile reports whether another account has a chance of
// succeeding. Quota and throttling errors qualify, and so does an answer that
// carried no image at all — whether it came back empty or as prose: it is a
// transient upstream miss that a fresh conversation usually clears (live-checked
// 2026-09-02, an immediate retry succeeded). A transient attachment upload
// failure qualifies too: /v1/images/edits sends the source image first, and a
// 5xx or reset on that upload says nothing about the image itself, so it used to
// fail the whole edit on the first stumble without trying anywhere else. A
// content-policy block, a *refused* attachment or a malformed request would fail
// the same way everywhere.
func imageFailoverWorthwhile(err error) bool {
	return IsRateLimited(err) || errors.Is(err, errImageQuotaRefused) || errors.Is(err, errImageServiceUnavailable) || errors.Is(err, chathub.ErrImageLimit) || errors.Is(err, chathub.ErrEmptyCompletion) || errors.Is(err, errImageNoResource) || errors.Is(err, chathub.ErrAttachmentUploadFailed)
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

// imageURLsFromResult collects the generated image URLs, falling back to a scan
// of the answer text when the upstream omits the structured field.
//
// There used to be a second fallback that JSON-walked res.RawResult. It could
// never fire: chathub returns early on any other value, so RawResult only ever
// reaches here as "" or the literal "Success", and neither is valid JSON. The
// text fallback was the same JSON walk, which is equally useless against prose —
// so the real case it was meant to cover (the upstream names the URL in the
// sentence instead of the structured field) was never actually handled.
func imageURLsFromResult(res chathub.Result) []string {
	if len(res.Images) > 0 {
		return res.Images
	}
	return imageURLsFromText(res.Text)
}

// textURLPattern matches an https URL inside prose. The character class is the
// RFC 3986 set on purpose: stopping at the first non-URL byte keeps a CJK
// sentence that abuts the URL ("…dalle-1.png。这是图片") from being swallowed into
// the match and making the extension test fail.
var textURLPattern = regexp.MustCompile(`https://[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+`)

// imageURLsFromText finds image URLs the upstream mentioned in its answer, in
// markdown (![alt](url)) or bare form.
func imageURLsFromText(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, candidate := range textURLPattern.FindAllString(text, -1) {
		// Trailing punctuation belongs to the sentence or the markdown wrapper,
		// not to the URL.
		candidate = strings.TrimRight(candidate, `.,;:!?*_~)]'`)
		if candidate == "" || seen[candidate] || !looksLikeImageURL(candidate) {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

// imageURLExtensionPattern accepts an extension that ends the URL or is followed
// by another query parameter, mirroring chathub's own predicate: Designer puts
// the filename in the query (?path=…/dalle-1.png&dcHint=…).
var imageURLExtensionPattern = regexp.MustCompile(`\.(png|jpe?g|webp|gif)(&|$)`)

func looksLikeImageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return false
	}
	p := strings.ToLower(u.Path + "?" + u.RawQuery)
	return strings.Contains(p, "image") || imageURLExtensionPattern.MatchString(p)
}

// markImageRefusalCooldown applies the image-only cooldown a natural-language
// refusal implies and returns the sentinel the failover loop should carry.
//
// The upstream answered HTTP 200 here, so chatWithAccount has already recorded a
// success for this account, which clears its general cooldown. That used to be
// called out as an ordering dependency ("our write lands after MarkSuccess"),
// which was both hard to verify and not the thing making it safe: MarkSuccess
// deliberately preserves an image-gen cooldown that is still in the future, so
// the mark survives whichever order the two writes land in. Keeping the
// refusal-to-cooldown mapping in one place makes that the only invariant to
// check, and TestImageRefusalCooldownSurvivesSuccess pins it.
func (s *Server) markImageRefusalCooldown(accountID string, refusal imageRefusalKind) error {
	switch refusal {
	case imageRefusalQuota:
		s.accountPool.MarkImageGenTokensThrottled(accountID)
		return errImageQuotaRefused
	case imageRefusalCapacity:
		// The upstream image service itself is down or overloaded rather than
		// this account being out of quota, so hold the account back briefly and
		// let another one try.
		s.accountPool.MarkImageGenSystemThrottled(accountID)
		return errImageServiceUnavailable
	}
	return nil
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
	// Only the success path used to reach the usage log, so a burst of image
	// failures was invisible both in the console and in per-key accounting —
	// exactly the requests an operator most wants to see. Wrap the writer once
	// and record on the way out, whichever of the dozen-plus exits is taken.
	tw := &traceWriter{ResponseWriter: w}
	w = tw
	usageRec := UsageRecord{
		APIKeyPrefix: extractAPIKey(r),
		ClientIP:     clientIP(r),
		Model:        "gpt-image-2",
		Endpoint:     r.URL.Path,
	}
	defer func() {
		if s.usage == nil {
			return
		}
		usageRec.Time = time.Now()
		usageRec.DurationMs = time.Since(startedAt).Milliseconds()
		usageRec.Status = tw.status
		if usageRec.Status == 0 {
			usageRec.Status = http.StatusOK
		}
		s.usage.record(usageRec)
	}()
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
	usageRec.Model = firstNonEmpty(b.Model, "gpt-image-2")
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
	usageRec.AccountEmail = acc.Email
	if acc.OID == "" || acc.TID == "" {
		acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "account missing oid/tid — re-login with PKCE")
		return
	}
	imageTimeout := time.Duration(s.settings.get().ImageTimeoutSeconds) * time.Second
	// ImageTimeoutSeconds used to be handed out per account attempt *and* again to
	// the download phase, each derived straight from r.Context(): three attempts
	// plus downloads could hold the connection for four times the configured
	// timeout — 600s at the default 150s, long after any client had stopped
	// listening (Claude CLI gives up near 125s). One budget now covers the whole
	// request and every phase derives from it, so a phase can only shorten the
	// wait, never extend it.
	requestCtx, cancelRequest := context.WithTimeout(r.Context(), imageRequestBudgetFactor*imageTimeout)
	defer cancelRequest()
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
	usageRec.Endpoint = endpoint
	usageRec.InputTokens = EstimateTokens(prompt)

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
		attemptCtx, cancel := context.WithTimeout(requestCtx, imageTimeout)
		res, lastErr = s.chatWithAccount(attemptCtx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{Text: prompt, Tone: "magic", Attachments: b.Attachments, LicenseType: s.settings.get().LicenseType, Scenario: s.settings.get().Scenario, FeatureFlags: s.featureFlags(), Capability: "ImageGeneration"})
		cancel()
		if lastErr == nil {
			log.Printf("[image-gen] conversation=%s images=%d text_len=%d events=%d raw_len=%d", res.ConversationID, len(res.Images), len(res.Text), len(res.Events), len(res.RawResult))
			images = imageURLsFromResult(res)
			if len(images) > 0 {
				break
			}
			switch kind := classifyImageRefusal(strings.Join([]string{res.Text, res.RawResult}, "\n")); kind {
			case imageRefusalQuota, imageRefusalCapacity:
				// The upstream answered 200 with a natural-language refusal, so
				// chatWithAccount already recorded a success and cleared this
				// account's cooldown. markImageRefusalCooldown is the single place a
				// refusal turns into an image-only cooldown, and MarkSuccess is what
				// keeps the mark alive; it also returns the sentinel the loop carries.
				if sentinel := s.markImageRefusalCooldown(acc.ID, kind); sentinel != nil {
					lastErr = sentinel
				}
			case imageRefusalPolicy:
				// Every account would refuse the same prompt; report it as the
				// client's problem so it does not retry and burn more quota.
				logImageGenDebug(res)
				writeContentPolicyBlocked(w, res.Text)
				return
			default:
				// The upstream answered but produced no image and gave no reason
				// we recognise. Let the loop try another account before failing:
				// returning here made one transient miss a hard 502.
				logImageGenDebug(res)
				lastErr = errImageNoResource
			}
		} else {
			s.markImageThrottle(acc.ID, lastErr)
		}
		if pinned || attempt >= imageAccountAttempts || !imageFailoverWorthwhile(lastErr) {
			break
		}
		// Rotating is only worth it while enough of the overall budget remains for
		// the next attempt to actually finish; otherwise it just delays the same
		// failure past the caller's own patience.
		if budget, enough := transportRetryBudget(requestCtx); !enough {
			log.Printf("[image-gen] endpoint=%s account=%s: %s left of the image budget, not rotating (%v)", endpoint, acc.ID, budget.Round(time.Second), lastErr)
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
	// Failover may have moved on from the account resolved up front; attribute
	// the request to the one that actually answered, success or failure.
	usageRec.AccountEmail = acc.Email
	if lastErr != nil {
		switch {
		case errors.Is(lastErr, errImageQuotaRefused):
			w.Header().Set("Retry-After", strconv.Itoa(s.imageRetryAfter(acc.ID)))
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "M365 image generation quota is exhausted; try again later or use another account")
		case errors.Is(lastErr, errImageServiceUnavailable):
			w.Header().Set("Retry-After", strconv.Itoa(s.imageRetryAfter(acc.ID)))
			writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_unavailable", "M365 image generation is temporarily unavailable upstream; retry shortly")
		case errors.Is(lastErr, errImageNoResource):
			// Every account tried answered without an image. Keep the historical
			// 502 shape, but it now means "the upstream really has nothing for
			// this prompt" rather than "the first attempt happened to miss".
			log.Printf("[image-gen] endpoint=%s accounts_tried=%d: no image resource", endpoint, len(tried))
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned no image resource")
		default:
			log.Printf("[image-gen] endpoint=%s account=%s failed: %v", endpoint, acc.ID, lastErr)
			writeUpstreamError(w, lastErr)
		}
		return
	}
	ctx, cancel := context.WithTimeout(requestCtx, imageDownloadBudget)
	defer cancel()
	if len(images) > b.N {
		images = images[:b.N]
	}

	var designerToken string
	// One unlucky image used to sink the whole response: every failure below
	// returned, discarding the images already downloaded. Collect what succeeds
	// and only report a failure when nothing could be delivered.
	var firstFailure *imageDeliveryFailure
	failures := 0
	note := func(status int, code, message string) {
		failures++
		if firstFailure == nil {
			firstFailure = &imageDeliveryFailure{status: status, code: code, message: message}
		}
	}
	data := make([]map[string]string, 0, len(images))
	for _, sourceURL := range images {
		if strings.HasPrefix(strings.ToLower(sourceURL), "data:image/") {
			if format == "b64_json" {
				parts := strings.SplitN(sourceURL, ",", 2)
				if len(parts) != 2 {
					note(http.StatusBadGateway, "upstream_error", "invalid upstream image data")
					continue
				}
				data = append(data, map[string]string{"b64_json": parts[1]})
			} else {
				data = append(data, map[string]string{"url": sourceURL})
			}
			continue
		}
		if !isDesignerImageURL(sourceURL) {
			if format == "b64_json" {
				note(http.StatusBadGateway, "unsupported_response_format", "upstream returned URL, not b64_json")
				continue
			}
			data = append(data, map[string]string{"url": sourceURL})
			continue
		}
		imageData, contentType, refreshedToken, err := s.fetchDesignerImage(ctx, sourceURL, designerToken, acc)
		designerToken = refreshedToken
		if err != nil {
			log.Printf("[image-gen-download] err=%v", err)
			status, code, message := designerDownloadFailure(err)
			note(status, code, message)
			continue
		}
		if format == "b64_json" {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(imageData)})
			continue
		}
		id := s.storeGeneratedImage(imageData, contentType)
		data = append(data, map[string]string{"url": generatedImageURL(r, id)})
	}
	if len(data) == 0 {
		if firstFailure == nil {
			// The upstream named images and every one of them was filtered out
			// without an error, which should not happen; report it rather than
			// answering 200 with an empty data array.
			firstFailure = &imageDeliveryFailure{status: http.StatusBadGateway, code: "upstream_error", message: "no image could be delivered"}
		}
		if firstFailure.status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		log.Printf("[image-gen] endpoint=%s account=%s: all %d image(s) failed delivery: %s", endpoint, acc.ID, len(images), firstFailure.message)
		writeOpenAIError(w, firstFailure.status, firstFailure.code, firstFailure.message)
		return
	}
	if failures > 0 {
		log.Printf("[image-gen] endpoint=%s account=%s: delivered %d/%d image(s), first failure: %s", endpoint, acc.ID, len(data), len(images), firstFailure.message)
	}

	jsonOut(w, map[string]any{"created": time.Now().Unix(), "data": data, "m365": map[string]any{"conversationId": res.ConversationID, "sessionId": res.SessionID, "images": images}})
}

func (s *Server) imageEdits(w http.ResponseWriter, r *http.Request) {
	// Early validation exits below return before imageGenerations, which owns the
	// usage recording for the body of the path. Without a traceWriter here a
	// rejected edit (bad multipart, oversized, missing prompt) would be invisible
	// in the usage log — the same gap the unified handler just closed. Record only
	// when we do not hand off; the forwarded request is recorded by
	// imageGenerations itself, so it must not be double-counted.
	startedAt := time.Now()
	tw := &traceWriter{ResponseWriter: w}
	w = tw
	usageRec := UsageRecord{
		APIKeyPrefix: extractAPIKey(r),
		ClientIP:     clientIP(r),
		Model:        "gpt-image-2",
		Endpoint:     "/v1/images/edits",
	}
	forwarded := false
	defer func() {
		if s.usage == nil || forwarded {
			return
		}
		usageRec.Time = time.Now()
		usageRec.DurationMs = time.Since(startedAt).Milliseconds()
		usageRec.Status = tw.status
		if usageRec.Status == 0 {
			usageRec.Status = http.StatusOK
		}
		s.usage.record(usageRec)
	}()
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
	forwarded = true
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

// designerDownloadError explains why a generated image could not be fetched.
// Every failure used to be an opaque error that the caller turned into a
// permanent 502, so a reset connection was indistinguishable from an image that
// will never be downloadable.
type designerDownloadError struct {
	// Status is the upstream HTTP status, or 0 for a transport-level failure.
	Status int
	// Retryable marks a failure that a later attempt at the same URL may clear.
	Retryable bool
	// Expired marks a rejected token rather than a bad URL: re-minting the
	// short-lived Designer token is what makes the next attempt useful.
	Expired bool
	// Public is the client-visible reason, already free of internal detail.
	Public string
	cause  error
}

func (e *designerDownloadError) Error() string {
	if e.cause == nil {
		return e.Public
	}
	if e.Status != 0 {
		return fmt.Sprintf("designer download HTTP %d: %v", e.Status, e.cause)
	}
	return fmt.Sprintf("designer download: %v", e.cause)
}

func (e *designerDownloadError) Unwrap() error { return e.cause }

// designerDownloadFailure maps a download failure onto a client-visible
// response. A transient fetch failure must not look like the permanent 502 this
// path used to return for everything, otherwise callers cannot tell that
// retrying would work.
func designerDownloadFailure(err error) (int, string, string) {
	var de *designerDownloadError
	if errors.As(err, &de) {
		if de.Retryable || de.Expired {
			return http.StatusServiceUnavailable, "upstream_unavailable", "the generated image could not be downloaded from M365 Designer; retry shortly"
		}
		return http.StatusBadGateway, "upstream_error", de.Public
	}
	return http.StatusBadGateway, "upstream_error", upstreamError(err)
}

func isRetryableDesignerError(err error) bool {
	var de *designerDownloadError
	return errors.As(err, &de) && de.Retryable
}

// fetchDesignerImage downloads one generated image, retrying transient upstream
// failures and re-minting the Designer token once if it was rejected. It returns
// the token it ended up using so the caller can keep reusing a refreshed one
// across the remaining images of the same request.
func (s *Server) fetchDesignerImage(ctx context.Context, rawURL, token string, acc auth.AccountToken) ([]byte, string, string, error) {
	var lastErr error
	refreshed := false
	backoff := designerRetryBackoff
	for attempt := 1; attempt <= designerDownloadAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, "", token, ctx.Err()
		}
		if token == "" {
			minted, err := s.designerAccessToken(acc)
			if err != nil {
				return nil, "", token, err
			}
			token = minted
		}
		data, contentType, err := downloadDesignerImage(ctx, rawURL, token)
		if err == nil {
			return data, contentType, token, nil
		}
		lastErr = err
		var de *designerDownloadError
		if errors.As(err, &de) && de.Expired && !refreshed {
			// The URL is still valid; only the short-lived token was rejected, so
			// re-mint it and go straight back in without waiting.
			log.Printf("[image-gen-download] designer token rejected (HTTP %d), re-minting", de.Status)
			refreshed = true
			token = ""
			continue
		}
		if attempt == designerDownloadAttempts || !isRetryableDesignerError(err) {
			break
		}
		log.Printf("[image-gen-download] attempt=%d/%d retrying: %v", attempt, designerDownloadAttempts, err)
		select {
		case <-ctx.Done():
			return nil, "", token, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, "", token, lastErr
}

func downloadDesignerImage(ctx context.Context, rawURL, accessToken string) ([]byte, string, error) {
	if !isDesignerImageURL(rawURL) {
		return nil, "", &designerDownloadError{Public: "the upstream returned a generated image on an unsupported host", cause: fmt.Errorf("unsupported generated image host")}
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
		// A reset connection or a proxy hiccup says nothing about whether the
		// image is fetchable, so the same URL is worth another attempt.
		return nil, "", &designerDownloadError{Retryable: true, Public: "the generated image could not be downloaded from M365 Designer", cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", classifyDesignerStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil {
		// The response started arriving and then broke off mid-body; a fresh
		// request is the only way to recover.
		return nil, "", &designerDownloadError{Status: resp.StatusCode, Retryable: true, Public: "the generated image download was interrupted", cause: err}
	}
	if len(body) > maxGeneratedImageBytes {
		return nil, "", &designerDownloadError{Status: resp.StatusCode, Public: fmt.Sprintf("the generated image exceeds the %d MiB download limit", maxGeneratedImageBytes>>20), cause: fmt.Errorf("generated image exceeds %d bytes", maxGeneratedImageBytes)}
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = http.DetectContentType(body)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return nil, "", &designerDownloadError{Status: resp.StatusCode, Public: "M365 Designer returned a non-image response for the generated image", cause: fmt.Errorf("Designer returned non-image content")}
	}
	return body, contentType, nil
}

// classifyDesignerStatus decides whether an upstream status is worth another
// attempt, needs a fresh token, or is final.
func classifyDesignerStatus(status int) *designerDownloadError {
	cause := fmt.Errorf("Designer image download HTTP %d", status)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &designerDownloadError{Status: status, Expired: true, Public: "the M365 Designer download token was rejected", cause: cause}
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return &designerDownloadError{Status: status, Retryable: true, Public: "M365 Designer is temporarily unable to serve the generated image", cause: cause}
	default:
		return &designerDownloadError{Status: status, Public: "M365 Designer rejected the generated image download", cause: cause}
	}
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
