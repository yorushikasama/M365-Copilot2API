package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

var ErrOffensiveContent = errors.New("upstream content policy flagged as offensive")

func logOAuthError(stage string, err error) {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		log.Printf("oauth_error stage=%s error=%q aadsts=%q http_status=%d correlation_id=%q trace_id=%q", stage, oauthErr.Code, oauthErr.AADSTS, oauthErr.HTTPStatus, oauthErr.CorrelationID, oauthErr.TraceID)
		return
	}
	log.Printf("oauth_error stage=%s error=%q", stage, "request_failed")
}

// upstreamError keeps transport details, including URLs and credentials, out
// of client-visible responses while retaining a server-side diagnostic.
func upstreamError(err error) string {
	if err == nil {
		return "upstream request failed"
	}
	log.Printf("upstream request failed: %v", err)
	return "upstream request failed"
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// everything else is 502. Unknown upstream failures must never leak internals.
func upstreamStatus(err error) int {
	if errors.Is(err, chathub.ErrAttachmentRejected) {
		return http.StatusBadRequest
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, chathub.ErrImageLimit) {
		return http.StatusTooManyRequests
	}
	if IsRateLimited(err) {
		return http.StatusTooManyRequests
	}
	if IsAuthFailure(err) {
		return http.StatusUnauthorized
	}
	cat := ClassifyError(err)
	switch cat {
	case CategoryUserBanned:
		return http.StatusForbidden
	case CategoryUserThrottled:
		return http.StatusTooManyRequests
	case CategoryInsufficientTokens:
		return http.StatusTooManyRequests
	case CategoryRetryable422:
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}

func applyM365Headers(w http.ResponseWriter, err error, accountID string) {
	cat := ClassifyError(err)
	if accountID != "" {
		w.Header().Set("X-M365-Account-Id", accountID)
	} else {
		w.Header().Set("X-M365-Account-Id", "")
	}
	w.Header().Set("X-M365-Proxy-Error", string(cat))
	if GlobalCircuitIsOpen() {
		remaining := int(time.Until(GlobalCircuitOpenUntil()).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-M365-Global-Circuit", fmt.Sprintf("open; retry-after=%d", remaining))
	} else {
		w.Header().Set("X-M365-Global-Circuit", "closed")
	}
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("X-M365-Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Duration(retry)*time.Second).Unix()))
	} else {
		switch cat {
		case CategoryQuota429:
			w.Header().Set("X-M365-Retry-After", "30")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(30*time.Second).Unix()))
		case CategoryOverload503:
			w.Header().Set("X-M365-Retry-After", "15")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(15*time.Second).Unix()))
		case CategoryAuthExpired401:
			w.Header().Set("X-M365-Retry-After", "120")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(2*time.Minute).Unix()))
		case CategoryForbidden403:
			w.Header().Set("X-M365-Retry-After", "86400")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(24*time.Hour).Unix()))
		}
	}
	if IsRateLimited(err) {
		w.Header().Set("X-M365-RateLimit-Remaining", "0")
	} else {
		w.Header().Set("X-M365-RateLimit-Remaining", "1")
	}
}

func writeUpstreamErrorWithAccount(w http.ResponseWriter, err error, accountID string) {
	applyM365Headers(w, err, accountID)
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	logUpstreamFailure(accountID, status, err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "30")
		}
		if w.Header().Get("X-M365-Retry-After") == "" {
			w.Header().Set("X-M365-Retry-After", w.Header().Get("Retry-After"))
		}
		if errors.Is(err, chathub.ErrImageLimit) {
			writeOpenAIError(w, status, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if errors.Is(err, chathub.ErrAttachmentRejected) {
		writeOpenAIError(w, status, "invalid_image", "the upstream image sanitizer rejected the attached image; re-export it as a plain JPEG or PNG, or try another image")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}

// streamErrorCode maps an upstream failure category to a stable SSE error code
// the caller can branch on. Category and request id ride alongside so the
// client can distinguish a silent upstream (upstream_timeout) from a quota
// limit (rate_limit_error) instead of a blanket rate_limit.
func streamErrorCode(cat ErrorCategory) string {
	switch cat {
	case CategoryQuota429:
		return "rate_limit_error"
	case CategoryAuthExpired401:
		return "authentication_error"
	case CategoryForbidden403:
		return "permission_error"
	case CategoryWSReadTimeout:
		return "upstream_timeout"
	case CategoryAttachmentRejected:
		return "invalid_image"
	default:
		return "upstream_error"
	}
}

// streamErrorEvent renders a mid-stream failure as an SSE error payload with
// the message sanitized and the diagnosable bits (code, category, request id)
// attached.
func streamErrorEvent(requestID string, err error) map[string]any {
	cat := ClassifyError(err)
	msg := upstreamError(err)
	if IsRateLimited(err) {
		msg = "upstream is rate limiting; try again shortly"
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		msg = "M365 content policy flagged this request as offensive"
	}
	if errors.Is(err, context.DeadlineExceeded) || cat == CategoryWSReadTimeout {
		msg = "the upstream took too long to respond and the stream was cut off; try again in a moment"
	}
	return map[string]any{"error": map[string]any{
		"message":    sanitizePublicInternalText(msg),
		"code":       streamErrorCode(cat),
		"category":   string(cat),
		"request_id": requestID,
	}}
}

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	cat := ClassifyError(err)
	switch cat {
	case CategoryQuota429, CategoryOverload503, CategoryRetryable422,
		CategorySOCKS5, CategoryDNS, CategoryTCP, CategoryTLS,
		CategoryWSHandshake, CategoryWSReadTimeout, CategoryUpstreamStructured,
		CategoryGlobalUnavailable:
		return true
	case CategoryForbidden403, CategoryAuthExpired401,
		CategoryUserBanned, CategoryClientCanceled:
		return false
	default:
		return false
	}
}

func ClassifyErrorCode(code string) ErrorCategory {
	switch code {
	case "ErrorUserBanned":
		return CategoryUserBanned
	case "ErrorUserThrottled":
		return CategoryUserThrottled
	case "InsufficientTokens":
		return CategoryInsufficientTokens
	case "ErrorDisallowedAADUser":
		return CategoryDesignerDisabled
	default:
		return CategoryUnknown
	}
}

// logUpstreamFailure records why a request is being failed. Without it the
// journal holds only the status code, which leaves a 429 or 502 impossible to
// diagnose after the fact.
func logUpstreamFailure(accountID string, status int, err error) {
	if err == nil {
		return
	}
	if accountID == "" {
		accountID = "-"
	}
	log.Printf("[upstream-fail] account=%s status=%d category=%s err=%v", accountID, status, ClassifyError(err), err)
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint for rate limits so clients can back off.
func writeUpstreamError(w http.ResponseWriter, err error) {
	applyM365Headers(w, err, "")
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	logUpstreamFailure("", status, err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "30")
		}
		if w.Header().Get("X-M365-Retry-After") == "" {
			w.Header().Set("X-M365-Retry-After", w.Header().Get("Retry-After"))
		}
		if errors.Is(err, chathub.ErrImageLimit) {
			writeOpenAIError(w, status, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if errors.Is(err, chathub.ErrAttachmentRejected) {
		writeOpenAIError(w, status, "invalid_image", "the upstream image sanitizer rejected the attached image; re-export it as a plain JPEG or PNG, or try another image")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}
