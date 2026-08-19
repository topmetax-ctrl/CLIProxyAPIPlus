package cursor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

const (
	DefaultQuotaAPIBase = "https://api2.cursor.sh"

	quotaCurrentPeriodPath = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	quotaPlanInfoPath      = "/aiserver.v1.DashboardService/GetPlanInfo"
	quotaCreditsPath       = "/aiserver.v1.DashboardService/GetCreditGrantsBalance"
	quotaAuthUsagePath     = "/auth/usage"
	quotaUsageSummaryPath  = "/api/usage/summary"

	QuotaSourceDashboard = "cursor-dashboard"
	QuotaSourceUsageAPI  = "cursor-usage-api"

	QuotaStatusOK           = "ok"
	QuotaStatusUnavailable  = "unavailable"
	QuotaStatusNotSupported = "not_supported"

	QuotaErrorFetch        = "quota_fetch_error"
	QuotaErrorSchema       = "quota_schema_changed"
	QuotaErrorTimeout      = "quota_timeout"
	QuotaErrorUnauthorized = "quota_unauthorized"
	QuotaErrorNotSupported = "quota_not_supported_for_plan"

	defaultQuotaCacheTTL    = 45 * time.Second
	defaultQuotaHTTPTimeout = 15 * time.Second
)

// QuotaSnapshot is the canonical, UI-stable view of a Cursor account's
// provider-side quota. Field names stay independent of Cursor's private
// DashboardService protobuf so a protocol bump only changes the client.
type QuotaSnapshot struct {
	Provider  string `json:"provider"`
	AccountID string `json:"accountId,omitempty"`
	Plan      string `json:"plan,omitempty"`
	PlanPrice string `json:"planPrice,omitempty"`
	Status    string `json:"status"`

	PeriodStart *time.Time `json:"periodStart,omitempty"`
	PeriodEnd   *time.Time `json:"periodEnd,omitempty"`

	Usage *PercentUsage `json:"usage,omitempty"`
	Auto  *PercentUsage `json:"auto,omitempty"`
	API   *PercentUsage `json:"api,omitempty"`

	Spend    *MoneyUsage     `json:"spend,omitempty"`
	Credits  *CreditBalance  `json:"credits,omitempty"`
	OnDemand *OnDemandUsage  `json:"onDemand,omitempty"`
	Requests []RequestBucket `json:"requests,omitempty"`

	DisplayMessage string      `json:"displayMessage,omitempty"`
	Error          *QuotaError `json:"error,omitempty"`
	Source         string      `json:"source"`
	FetchedAt      time.Time   `json:"fetchedAt"`
	Stale          bool        `json:"stale"`
}

// PercentUsage is a 0–100 utilization reading.
type PercentUsage struct {
	UsedPercent      float64 `json:"usedPercent"`
	RemainingPercent float64 `json:"remainingPercent"`
}

// MoneyUsage is included-plan spend in USD cents.
type MoneyUsage struct {
	UsedCents      int64 `json:"usedCents"`
	LimitCents     int64 `json:"limitCents"`
	RemainingCents int64 `json:"remainingCents"`
	IncludedCents  int64 `json:"includedCents,omitempty"`
	BonusCents     int64 `json:"bonusCents,omitempty"`
}

// CreditBalance combines promotional grants with prepaid funds when present.
type CreditBalance struct {
	GrantedCents   int64 `json:"grantedCents"`
	PrepaidCents   int64 `json:"prepaidCents"`
	RemainingCents int64 `json:"remainingCents"`
}

// OnDemandUsage is the post-plan spend cap, user- or team-scoped.
type OnDemandUsage struct {
	LimitType            string `json:"limitType,omitempty"`
	UsedCents            int64  `json:"usedCents"`
	LimitCents           int64  `json:"limitCents,omitempty"`
	RemainingCents       int64  `json:"remainingCents,omitempty"`
	PooledLimitCents     int64  `json:"pooledLimitCents,omitempty"`
	IndividualLimitCents int64  `json:"individualLimitCents,omitempty"`
}

// RequestBucket is an enterprise-style per-model request allowance.
type RequestBucket struct {
	Model     string `json:"model"`
	Used      int64  `json:"used"`
	Limit     int64  `json:"limit"`
	Remaining int64  `json:"remaining"`
}

// QuotaError is a quota-fetch failure. It must never be treated as
// credential health: a DashboardService protocol change does not mean
// AgentService/Run will fail.
type QuotaError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *QuotaError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// RemainingPercent prefers the plan usage remaining reading, then spend
// remaining, then enterprise request remaining. Used by future quota-aware
// routing; callers must treat a missing value as "unknown", not zero.
func (s *QuotaSnapshot) RemainingPercent() (float64, bool) {
	if s == nil || s.Status != QuotaStatusOK {
		return 0, false
	}
	if s.Usage != nil {
		return s.Usage.RemainingPercent, true
	}
	if s.Spend != nil && s.Spend.LimitCents > 0 {
		return 100 * float64(s.Spend.RemainingCents) / float64(s.Spend.LimitCents), true
	}
	var used, limit int64
	for _, bucket := range s.Requests {
		used += bucket.Used
		limit += bucket.Limit
	}
	if limit > 0 {
		return 100 * float64(limit-used) / float64(limit), true
	}
	return 0, false
}

// FetchQuotaOptions selects the credential and cache behaviour for one fetch.
type FetchQuotaOptions struct {
	CacheKey     string
	AccountID    string
	AccessToken  string
	RefreshToken string
	SkipCache    bool
	Transport    http.RoundTripper
	// OnRefreshed is invoked when a 401/expiry forces a token refresh so the
	// caller can persist the new pair. Refresh failure is a quota error, not
	// credential invalidation.
	OnRefreshed func(accessToken, refreshToken string)
}

type quotaCacheEntry struct {
	snapshot QuotaSnapshot
	expires  time.Time
}

// QuotaClient talks to Cursor's private DashboardService using Connect-JSON.
// Optional endpoints (plan info, credits, enterprise usage) never fail a
// successful period-usage read.
type QuotaClient struct {
	BaseURL  string
	HTTP     *http.Client
	CacheTTL time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]quotaCacheEntry
	group singleflight.Group
}

// NewQuotaClient returns a client with a 45s cache and 15s HTTP timeout.
// The timeout applies to credential-adjacent management fetches only.
func NewQuotaClient() *QuotaClient {
	return &QuotaClient{
		BaseURL:  DefaultQuotaAPIBase,
		HTTP:     &http.Client{Timeout: defaultQuotaHTTPTimeout},
		CacheTTL: defaultQuotaCacheTTL,
		now:      time.Now,
		cache:    make(map[string]quotaCacheEntry),
	}
}

var defaultQuotaClient = NewQuotaClient()

// FetchQuota is the package-level entry used by the management API.
func FetchQuota(ctx context.Context, opts FetchQuotaOptions) QuotaSnapshot {
	return defaultQuotaClient.Fetch(ctx, opts)
}

// Fetch returns a snapshot for one credential. Concurrent callers for the
// same CacheKey coalesce into a single upstream round-trip.
func (c *QuotaClient) Fetch(ctx context.Context, opts FetchQuotaOptions) QuotaSnapshot {
	if c == nil {
		return unavailableSnapshot(opts.AccountID, QuotaErrorFetch, "quota client is nil", false)
	}
	if strings.TrimSpace(opts.AccessToken) == "" && strings.TrimSpace(opts.RefreshToken) == "" {
		return unavailableSnapshot(opts.AccountID, QuotaErrorUnauthorized, "missing Cursor access token", false)
	}

	now := c.nowFn()
	key := strings.TrimSpace(opts.CacheKey)
	if key == "" {
		key = strings.TrimSpace(opts.AccountID)
	}
	if key == "" {
		key = "anon"
	}

	if !opts.SkipCache {
		if cached, ok := c.cached(key, now); ok {
			return cached
		}
	}

	v, _, _ := c.group.Do(key, func() (any, error) {
		if !opts.SkipCache {
			if cached, ok := c.cached(key, c.nowFn()); ok {
				return cached, nil
			}
		}
		snap := c.fetchUncached(ctx, opts)
		if snap.Status == QuotaStatusOK {
			c.store(key, snap)
			return snap, nil
		}
		if stale, ok := c.cachedIncludingStale(key); ok {
			stale.Stale = true
			stale.Error = snap.Error
			if snap.Error != nil {
				stale.Status = QuotaStatusUnavailable
			}
			return stale, nil
		}
		return snap, nil
	})
	if snap, ok := v.(QuotaSnapshot); ok {
		return snap
	}
	return unavailableSnapshot(opts.AccountID, QuotaErrorFetch, "unexpected quota cache result", false)
}

func (c *QuotaClient) fetchUncached(ctx context.Context, opts FetchQuotaOptions) QuotaSnapshot {
	token := strings.TrimSpace(opts.AccessToken)
	refresh := strings.TrimSpace(opts.RefreshToken)
	if token != "" && refresh != "" && !GetTokenExpiry(token).After(c.nowFn()) {
		if pair, err := RefreshToken(ctx, refresh); err == nil && pair != nil && pair.AccessToken != "" {
			token = pair.AccessToken
			if pair.RefreshToken != "" {
				refresh = pair.RefreshToken
			}
			if opts.OnRefreshed != nil {
				opts.OnRefreshed(token, refresh)
			}
		}
	}

	usageBody, err := c.connectJSON(ctx, token, quotaCurrentPeriodPath, opts.Transport)
	if isUnauthorized(err) && refresh != "" {
		pair, refreshErr := RefreshToken(ctx, refresh)
		if refreshErr != nil || pair == nil || pair.AccessToken == "" {
			return unavailableSnapshot(opts.AccountID, QuotaErrorUnauthorized, "Cursor dashboard rejected the access token", false)
		}
		token = pair.AccessToken
		if pair.RefreshToken != "" {
			refresh = pair.RefreshToken
		}
		if opts.OnRefreshed != nil {
			opts.OnRefreshed(token, refresh)
		}
		usageBody, err = c.connectJSON(ctx, token, quotaCurrentPeriodPath, opts.Transport)
	}
	if err != nil {
		return snapshotFromFetchError(opts.AccountID, err)
	}

	snap, ok := parseCurrentPeriodUsage(usageBody)
	if !ok {
		// Individual/team period usage missing: try enterprise request buckets.
		if authBody, authErr := c.getJSON(ctx, token, quotaAuthUsagePath, opts.Transport); authErr == nil {
			if requests, reqOK := parseAuthUsage(authBody); reqOK {
				snap = QuotaSnapshot{
					Provider: "cursor",
					Status:   QuotaStatusOK,
					Requests: requests,
					Source:   QuotaSourceUsageAPI,
				}
				ok = true
			}
		}
	}
	if !ok {
		return unavailableSnapshot(opts.AccountID, QuotaErrorSchema, "Cursor dashboard usage payload did not match a known schema", false)
	}

	snap.Provider = "cursor"
	snap.AccountID = opts.AccountID
	snap.FetchedAt = c.nowFn().UTC()
	if snap.Source == "" {
		snap.Source = QuotaSourceDashboard
	}
	if snap.Status == "" {
		snap.Status = QuotaStatusOK
	}

	if planBody, planErr := c.connectJSON(ctx, token, quotaPlanInfoPath, opts.Transport); planErr == nil {
		applyPlanInfo(&snap, planBody)
	}
	if creditBody, creditErr := c.connectJSON(ctx, token, quotaCreditsPath, opts.Transport); creditErr == nil {
		applyCreditGrants(&snap, creditBody)
	}
	if summaryBody, summaryErr := c.getJSON(ctx, token, quotaUsageSummaryPath, opts.Transport); summaryErr == nil {
		applyUsageSummary(&snap, summaryBody)
	}
	return snap
}

func (c *QuotaClient) connectJSON(ctx context.Context, accessToken, path string, rt http.RoundTripper) ([]byte, error) {
	return c.doJSON(ctx, http.MethodPost, path, accessToken, []byte("{}"), rt)
}

func (c *QuotaClient) getJSON(ctx context.Context, accessToken, path string, rt http.RoundTripper) ([]byte, error) {
	return c.doJSON(ctx, http.MethodGet, path, accessToken, nil, rt)
}

func (c *QuotaClient) doJSON(ctx context.Context, method, path, accessToken string, body []byte, rt http.RoundTripper) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultQuotaAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	client := c.httpClient(rt)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return payload, &QuotaError{Code: QuotaErrorUnauthorized, Message: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return payload, &QuotaError{Code: QuotaErrorFetch, Message: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	if connectErr := connectProtocolError(payload); connectErr != nil {
		return payload, connectErr
	}
	return payload, nil
}

func (c *QuotaClient) httpClient(rt http.RoundTripper) *http.Client {
	timeout := defaultQuotaHTTPTimeout
	if c != nil && c.HTTP != nil && c.HTTP.Timeout > 0 {
		timeout = c.HTTP.Timeout
	}
	if rt != nil {
		return &http.Client{Timeout: timeout, Transport: rt}
	}
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: timeout}
}

func (c *QuotaClient) nowFn() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *QuotaClient) ttl() time.Duration {
	if c != nil && c.CacheTTL > 0 {
		return c.CacheTTL
	}
	return defaultQuotaCacheTTL
}

func (c *QuotaClient) cached(key string, now time.Time) (QuotaSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok || now.After(entry.expires) {
		return QuotaSnapshot{}, false
	}
	return entry.snapshot, true
}

func (c *QuotaClient) cachedIncludingStale(key string) (QuotaSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok {
		return QuotaSnapshot{}, false
	}
	return entry.snapshot, true
}

func (c *QuotaClient) store(key string, snap QuotaSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]quotaCacheEntry)
	}
	c.cache[key] = quotaCacheEntry{snapshot: snap, expires: c.nowFn().Add(c.ttl())}
}

func snapshotFromFetchError(accountID string, err error) QuotaSnapshot {
	code := QuotaErrorFetch
	msg := "failed to fetch Cursor quota"
	if err != nil {
		msg = err.Error()
	}
	var qerr *QuotaError
	if errors.As(err, &qerr) && qerr != nil {
		code = qerr.Code
		msg = qerr.Message
	} else if isTimeout(err) {
		code = QuotaErrorTimeout
		msg = "Cursor dashboard quota request timed out"
	}
	return unavailableSnapshot(accountID, code, msg, false)
}

func unavailableSnapshot(accountID, code, message string, stale bool) QuotaSnapshot {
	status := QuotaStatusUnavailable
	if code == QuotaErrorNotSupported {
		status = QuotaStatusNotSupported
	}
	return QuotaSnapshot{
		Provider:  "cursor",
		AccountID: accountID,
		Status:    status,
		Error:     &QuotaError{Code: code, Message: message},
		Source:    QuotaSourceDashboard,
		FetchedAt: time.Now().UTC(),
		Stale:     stale,
	}
}

func isUnauthorized(err error) bool {
	var qerr *QuotaError
	return errors.As(err, &qerr) && qerr != nil && qerr.Code == QuotaErrorUnauthorized
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func connectProtocolError(body []byte) error {
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "code").String()))
	}
	if code == "" {
		return nil
	}
	msg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if msg == "" {
		msg = strings.TrimSpace(gjson.GetBytes(body, "message").String())
	}
	if strings.Contains(code, "unauth") || code == "permission_denied" {
		return &QuotaError{Code: QuotaErrorUnauthorized, Message: firstNonEmpty(msg, code)}
	}
	return &QuotaError{Code: QuotaErrorFetch, Message: firstNonEmpty(msg, code)}
}

func parseCurrentPeriodUsage(body []byte) (QuotaSnapshot, bool) {
	root := gjson.ParseBytes(body)
	plan := root.Get("planUsage")
	if !plan.Exists() {
		plan = root.Get("plan_usage")
	}
	spendLimit := root.Get("spendLimitUsage")
	if !spendLimit.Exists() {
		spendLimit = root.Get("spend_limit_usage")
	}

	snap := QuotaSnapshot{Provider: "cursor", Source: QuotaSourceDashboard, Status: QuotaStatusOK}
	snap.PeriodStart = parseUnixMillis(firstResult(root, "billingCycleStart", "billing_cycle_start"))
	snap.PeriodEnd = parseUnixMillis(firstResult(root, "billingCycleEnd", "billing_cycle_end"))
	snap.DisplayMessage = firstNonEmpty(root.Get("displayMessage").String(), root.Get("display_message").String())

	havePlan := plan.Exists() && plan.Type == gjson.JSON
	if havePlan {
		if used := jsonFloat(plan, "totalPercentUsed", "total_percent_used"); used != nil {
			snap.Usage = percentFromUsed(*used)
		}
		if used := jsonFloat(plan, "autoPercentUsed", "auto_percent_used"); used != nil {
			snap.Auto = percentFromUsed(*used)
		}
		if used := jsonFloat(plan, "apiPercentUsed", "api_percent_used"); used != nil {
			snap.API = percentFromUsed(*used)
		}
		limit := jsonInt(plan, "limit")
		included := jsonInt(plan, "includedSpend", "included_spend")
		remaining := jsonInt(plan, "remaining")
		totalSpend := jsonInt(plan, "totalSpend", "total_spend")
		if limit != nil || included != nil || remaining != nil || totalSpend != nil {
			money := &MoneyUsage{}
			if totalSpend != nil {
				money.UsedCents = *totalSpend
			} else if included != nil {
				money.UsedCents = *included
			}
			if included != nil {
				money.IncludedCents = *included
			}
			if bonus := jsonInt(plan, "bonusSpend", "bonus_spend"); bonus != nil {
				money.BonusCents = *bonus
			}
			if limit != nil {
				money.LimitCents = *limit
			}
			if remaining != nil {
				money.RemainingCents = *remaining
			} else if money.LimitCents > 0 {
				money.RemainingCents = money.LimitCents - money.IncludedCents
				if money.RemainingCents < 0 {
					money.RemainingCents = 0
				}
			}
			snap.Spend = money
			if snap.Usage == nil && money.LimitCents > 0 {
				usedPct := 100 * float64(money.IncludedCents) / float64(money.LimitCents)
				snap.Usage = percentFromUsed(usedPct)
			}
		}
	}

	if spendLimit.Exists() && spendLimit.Type == gjson.JSON {
		onDemand := &OnDemandUsage{
			LimitType: strings.TrimSpace(firstNonEmpty(spendLimit.Get("limitType").String(), spendLimit.Get("limit_type").String())),
		}
		if v := jsonInt(spendLimit, "totalSpend", "total_spend"); v != nil {
			onDemand.UsedCents = *v
		}
		if v := jsonInt(spendLimit, "individualUsed", "individual_used"); v != nil && onDemand.UsedCents == 0 {
			onDemand.UsedCents = *v
		}
		if v := jsonInt(spendLimit, "individualLimit", "individual_limit"); v != nil {
			onDemand.IndividualLimitCents = *v
			onDemand.LimitCents = *v
		}
		if v := jsonInt(spendLimit, "pooledLimit", "pooled_limit"); v != nil {
			onDemand.PooledLimitCents = *v
			if onDemand.LimitCents == 0 || strings.EqualFold(onDemand.LimitType, "team") {
				onDemand.LimitCents = *v
			}
		}
		if v := jsonInt(spendLimit, "individualRemaining", "individual_remaining"); v != nil {
			onDemand.RemainingCents = *v
		} else if v := jsonInt(spendLimit, "pooledRemaining", "pooled_remaining"); v != nil && onDemand.RemainingCents == 0 {
			onDemand.RemainingCents = *v
		} else if onDemand.LimitCents > 0 {
			onDemand.RemainingCents = onDemand.LimitCents - onDemand.UsedCents
			if onDemand.RemainingCents < 0 {
				onDemand.RemainingCents = 0
			}
		}
		if onDemand.LimitCents > 0 || onDemand.UsedCents > 0 {
			snap.OnDemand = onDemand
		}
	}

	if !havePlan && snap.OnDemand == nil {
		return QuotaSnapshot{}, false
	}
	return snap, true
}

func parseAuthUsage(body []byte) ([]RequestBucket, bool) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, false
	}
	var buckets []RequestBucket
	root.ForEach(func(key, value gjson.Result) bool {
		if !value.IsObject() {
			return true
		}
		used := jsonInt(value, "numRequests", "num_requests", "used")
		limit := jsonInt(value, "maxRequestUsage", "max_request_usage", "limit")
		if used == nil && limit == nil {
			return true
		}
		bucket := RequestBucket{Model: key.String()}
		if used != nil {
			bucket.Used = *used
		}
		if limit != nil {
			bucket.Limit = *limit
			bucket.Remaining = *limit - bucket.Used
			if bucket.Remaining < 0 {
				bucket.Remaining = 0
			}
		}
		buckets = append(buckets, bucket)
		return true
	})
	return buckets, len(buckets) > 0
}

func applyPlanInfo(snap *QuotaSnapshot, body []byte) {
	if snap == nil {
		return
	}
	info := gjson.ParseBytes(body).Get("planInfo")
	if !info.Exists() {
		info = gjson.ParseBytes(body).Get("plan_info")
	}
	if !info.Exists() {
		info = gjson.ParseBytes(body)
	}
	if name := strings.TrimSpace(firstNonEmpty(info.Get("planName").String(), info.Get("plan_name").String())); name != "" {
		snap.Plan = name
	}
	if price := strings.TrimSpace(info.Get("price").String()); price != "" {
		snap.PlanPrice = price
	}
	if snap.PeriodEnd == nil {
		snap.PeriodEnd = parseUnixMillis(firstResult(info, "billingCycleEnd", "billing_cycle_end"))
	}
	if snap.Spend != nil && snap.Spend.LimitCents == 0 {
		if included := jsonInt(info, "includedAmountCents", "included_amount_cents"); included != nil {
			snap.Spend.LimitCents = *included
			if snap.Spend.RemainingCents == 0 && snap.Spend.LimitCents > snap.Spend.IncludedCents {
				snap.Spend.RemainingCents = snap.Spend.LimitCents - snap.Spend.IncludedCents
			}
		}
	}
}

func applyCreditGrants(snap *QuotaSnapshot, body []byte) {
	if snap == nil {
		return
	}
	root := gjson.ParseBytes(body)
	credits := &CreditBalance{}
	if v := jsonInt(root, "remainingCents", "remaining_cents", "balance.remainingCents", "balance.remaining_cents"); v != nil {
		credits.RemainingCents = *v
	}
	if v := jsonInt(root, "grantedCents", "granted_cents", "totalCents", "total_cents", "balance.grantedCents", "balance.totalCents"); v != nil {
		credits.GrantedCents = *v
	}
	if v := jsonInt(root, "prepaidCents", "prepaid_cents", "balance.prepaidCents"); v != nil {
		credits.PrepaidCents = *v
	}
	if credits.RemainingCents == 0 && credits.GrantedCents == 0 && credits.PrepaidCents == 0 {
		return
	}
	if credits.RemainingCents == 0 {
		credits.RemainingCents = credits.GrantedCents + credits.PrepaidCents
	}
	snap.Credits = credits
}

func applyUsageSummary(snap *QuotaSnapshot, body []byte) {
	if snap == nil || body == nil {
		return
	}
	root := gjson.ParseBytes(body)
	if snap.OnDemand == nil {
		if used := jsonInt(root, "onDemandSpendCents", "on_demand_spend_cents", "onDemand.usedCents"); used != nil {
			snap.OnDemand = &OnDemandUsage{UsedCents: *used}
			if limit := jsonInt(root, "onDemandLimitCents", "on_demand_limit_cents", "onDemand.limitCents"); limit != nil {
				snap.OnDemand.LimitCents = *limit
				snap.OnDemand.RemainingCents = *limit - *used
				if snap.OnDemand.RemainingCents < 0 {
					snap.OnDemand.RemainingCents = 0
				}
			}
		}
	}
}

func percentFromUsed(used float64) *PercentUsage {
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return &PercentUsage{UsedPercent: used, RemainingPercent: 100 - used}
}

func parseUnixMillis(v gjson.Result) *time.Time {
	if !v.Exists() || v.Type == gjson.Null {
		return nil
	}
	var ms int64
	switch v.Type {
	case gjson.Number:
		ms = v.Int()
	case gjson.String:
		s := strings.TrimSpace(v.String())
		if s == "" {
			return nil
		}
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			if ts, timeErr := time.Parse(time.RFC3339, s); timeErr == nil {
				utc := ts.UTC()
				return &utc
			}
			return nil
		}
		ms = parsed
	default:
		return nil
	}
	if ms <= 0 {
		return nil
	}
	if ms < 1e12 {
		ms *= 1000
	}
	ts := time.UnixMilli(ms).UTC()
	return &ts
}

func jsonInt(root gjson.Result, paths ...string) *int64 {
	for _, path := range paths {
		v := root.Get(path)
		if !v.Exists() || v.Type == gjson.Null {
			continue
		}
		var n int64
		switch v.Type {
		case gjson.Number:
			n = v.Int()
		case gjson.String:
			s := strings.TrimSpace(v.String())
			if s == "" {
				continue
			}
			parsed, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				f, ferr := strconv.ParseFloat(s, 64)
				if ferr != nil {
					continue
				}
				n = int64(f)
			} else {
				n = parsed
			}
		default:
			continue
		}
		return &n
	}
	return nil
}

func jsonFloat(root gjson.Result, paths ...string) *float64 {
	for _, path := range paths {
		v := root.Get(path)
		if !v.Exists() || v.Type == gjson.Null {
			continue
		}
		var n float64
		switch v.Type {
		case gjson.Number:
			n = v.Float()
		case gjson.String:
			s := strings.TrimSpace(v.String())
			if s == "" {
				continue
			}
			parsed, err := strconv.ParseFloat(s, 64)
			if err != nil {
				continue
			}
			n = parsed
		default:
			continue
		}
		return &n
	}
	return nil
}

func firstResult(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		v := root.Get(path)
		if v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ResetQuotaCacheForTest drops the default client's cache. Tests only.
func ResetQuotaCacheForTest() {
	defaultQuotaClient.mu.Lock()
	defaultQuotaClient.cache = make(map[string]quotaCacheEntry)
	defaultQuotaClient.mu.Unlock()
}
