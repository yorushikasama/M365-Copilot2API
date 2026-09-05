package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

var ErrOffensiveContent = errors.New("upstream content policy flagged as offensive")

// statusClientClosedRequest is nginx's 499. It is not in net/http because it is
// not an IANA code, but it is the de-facto way to record "the caller hung up
// before we answered".
//
// This matters in practice: Claude CLI abandons /v1/messages at roughly 125s
// while M365_CHAT_TIMEOUT_SECONDS defaults to 300, so a slow-but-healthy
// upstream produced a steady stream of 502s in the access log. A 502 says "the
// upstream is broken" and pollutes error-rate dashboards; 499 correctly says
// "nobody was left to receive the answer".
const statusClientClosedRequest = 499

// IsClientCanceled reports whether the request failed only because the caller
// went away (or the surrounding context was canceled). Such a failure is not an
// upstream fault: it must not be counted against the account, the global circuit
// breaker, or the gateway's own error rate.
func IsClientCanceled(err error) bool {
	if err == nil {
		return false
	}
	return ClassifyError(err) == CategoryClientCanceled
}

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
	// A client hanging up is not an upstream failure; logging it as one buried
	// the real faults under Claude CLI's ~125s cancellations.
	if IsClientCanceled(err) {
		return "client closed the request before the upstream answered"
	}
	log.Printf("upstream request failed: %v", err)
	return "upstream request failed"
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// a caller that hung up becomes 499, everything else is 502. Unknown upstream
// failures must never leak internals.
func upstreamStatus(err error) int {
	if errors.Is(err, chathub.ErrAttachmentRejected) {
		return http.StatusBadRequest
	}
	// Checked before every upstream classification: when the caller is gone the
	// upstream's own state is irrelevant and unknowable.
	if IsClientCanceled(err) {
		return statusClientClosedRequest
	}
	// The gateway never reached an upstream, so there is no bad gateway to
	// report: the service simply is not configured to serve this request yet.
	if errors.Is(err, errNoAccounts) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		return contentPolicyStatus
	}
	// A failed *upload* is the upstream being briefly unable to accept the file,
	// not a bad gateway: retrying works, so say so. ErrAttachmentRejected above
	// is the opposite case and stays a 400.
	if errors.Is(err, chathub.ErrAttachmentUploadFailed) {
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
	if status == statusClientClosedRequest {
		// The body almost certainly goes nowhere, but the status must be recorded
		// so the access log and any middleware see 499 rather than 502.
		writeOpenAIError(w, status, "client_closed_request", "client closed the request before the upstream answered")
		return
	}
	if errors.Is(err, errNoAccounts) {
		// Name the operator action instead of an opaque upstream failure: no
		// amount of client retrying adds an account.
		writeOpenAIError(w, status, "no_account_available", err.Error()+"; sign an account in from the admin console, or enable one that is disabled")
		return
	}
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
	if errors.Is(err, chathub.ErrAttachmentUploadFailed) {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "5")
		}
		writeOpenAIError(w, status, "upstream_unavailable", "the upstream could not accept the attached image right now; retry shortly")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeContentPolicyBlocked(w, "")
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}

// A content-policy refusal used to be reported two different ways: the image
// path answered 400 content_policy_violation while every chat path answered 503
// upstream_content_blocked. Same upstream verdict, opposite advice — and the 503
// was the misleading one, because no path actually retries a refusal on another
// account, so SDKs that honour 503 just re-sent the identical prompt and burned
// more quota for the identical refusal. These three declarations are now the one
// place that decides the shape.
const (
	contentPolicyErrorCode = "content_policy_violation"
	contentPolicyStatus    = http.StatusBadRequest
)

// contentPolicyMessage prefers the upstream's own wording, which usually says
// what it objected to, and falls back to a generic line.
func contentPolicyMessage(detail string) string {
	if detail = strings.TrimSpace(detail); detail != "" {
		return detail
	}
	return "M365 content policy refused this request; rephrase the prompt or try another account"
}

// writeContentPolicyBlocked renders a content-policy refusal as the terminal
// client error it is.
func writeContentPolicyBlocked(w http.ResponseWriter, detail string) {
	writeOpenAIError(w, contentPolicyStatus, contentPolicyErrorCode, contentPolicyMessage(detail))
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
	case CategoryClientCanceled:
		return "client_closed_request"
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
	code := streamErrorCode(cat)
	if IsRateLimited(err) {
		msg = "upstream is rate limiting; try again shortly"
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		// The category stays UPSTREAM_STRUCTURED so cooldowns are unaffected,
		// but the code has to name the refusal: an SSE consumer could not tell a
		// content block from any other upstream fault.
		msg = contentPolicyMessage("")
		code = contentPolicyErrorCode
	}
	if errors.Is(err, context.DeadlineExceeded) || cat == CategoryWSReadTimeout {
		msg = "the upstream took too long to respond and the stream was cut off; try again in a moment"
	}
	return map[string]any{"error": map[string]any{
		"message":    sanitizePublicInternalText(msg),
		"code":       code,
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
		CategoryUserBanned, CategoryClientCanceled,
		// Retrying cannot conjure an account; only an operator can.
		CategoryNoAccount:
		return false
	default:
		return false
	}
}

// minTransportRetryBudget is the least time that must remain on a request
// before re-issuing it upstream is worth attempting. Below it the second turn
// would be cut short by the caller's own deadline and only delay the failure.
const minTransportRetryBudget = 20 * time.Second

// transportRetryBudget reports how much of ctx's deadline is left, and whether
// enough of it remains for a second upstream attempt. A ctx without a deadline
// is unbounded, so it reports no measurable budget but is always worth retrying.
func transportRetryBudget(ctx context.Context) (time.Duration, bool) {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0, true
	}
	remain := time.Until(dl)
	return remain, remain >= minTransportRetryBudget
}

// shouldFailoverTransport reports whether a failed turn may be re-issued on
// another account purely because the transport faulted, plus the budget left on
// the caller's deadline.
//
// WS_READ_TIMEOUT has always been classified retryable, but only 429/401 ever
// reached the failover path, so the first slow or half-open upstream surfaced as
// a hard 502. Two conditions make acting on it correct: nothing may have reached
// the caller yet, which IsSafeToRetry carries over from the transport, and
// enough budget must remain for a fresh turn -- once the first attempt has burned
// the whole chat timeout, a second one only doubles the wait before failing the
// same way.
func shouldFailoverTransport(ctx context.Context, err error) (time.Duration, bool) {
	budget, enough := transportRetryBudget(ctx)
	if !enough || !IsRetryable(err) || !chathub.IsSafeToRetry(err) {
		return budget, false
	}
	return budget, true
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
	// Keep client hang-ups out of [upstream-fail]: they are routine (Claude CLI
	// gives up at ~125s) and would otherwise drown out genuine upstream faults.
	if status == statusClientClosedRequest {
		log.Printf("[client-canceled] account=%s status=%d err=%v", accountID, status, err)
		return
	}
	log.Printf("[upstream-fail] account=%s status=%d category=%s err=%v", accountID, status, ClassifyError(err), err)
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint for rate limits so clients can back off.
//
// It is the accountless form of writeUpstreamErrorWithAccount. The two used to
// be line-for-line copies, which meant every change to the error mapping had to
// be made twice — and the 499 handling above is exactly the kind of edit that
// gets applied to only one copy.
func writeUpstreamError(w http.ResponseWriter, err error) {
	writeUpstreamErrorWithAccount(w, err, "")
}
