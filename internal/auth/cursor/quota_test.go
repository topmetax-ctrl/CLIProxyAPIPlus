package cursor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const fixturePeriodUsage = `{
  "billingCycleStart": "1768399334000",
  "billingCycleEnd": "1771077734000",
  "planUsage": {
    "totalSpend": 23222,
    "includedSpend": 23222,
    "bonusSpend": 0,
    "remaining": 16778,
    "limit": 40000,
    "autoPercentUsed": 10.5,
    "apiPercentUsed": 46.444,
    "totalPercentUsed": 58.055
  },
  "spendLimitUsage": {
    "totalSpend": 1200,
    "pooledLimit": 50000,
    "pooledUsed": 0,
    "pooledRemaining": 50000,
    "individualLimit": 10000,
    "individualUsed": 1200,
    "individualRemaining": 8800,
    "limitType": "user"
  },
  "displayMessage": "You've used 58% of your usage limit"
}`

const fixturePlanInfo = `{
  "planInfo": {
    "planName": "Ultra",
    "includedAmountCents": 40000,
    "price": "$200/mo",
    "billingCycleEnd": "1771077734000"
  }
}`

const fixtureCredits = `{
  "grantedCents": 1500,
  "prepaidCents": 250,
  "remainingCents": 1750
}`

const fixtureEnterpriseUsage = `{
  "gpt-4": {"numRequests": 12, "maxRequestUsage": 500},
  "claude-3.5-sonnet": {"num_requests": 3, "max_request_usage": 50}
}`

func TestParseCurrentPeriodUsageContract(t *testing.T) {
	snap, ok := parseCurrentPeriodUsage([]byte(fixturePeriodUsage))
	if !ok {
		t.Fatal("expected known DashboardService schema")
	}
	if snap.Usage == nil || snap.Usage.UsedPercent < 58 || snap.Usage.UsedPercent > 59 {
		t.Fatalf("usage = %+v", snap.Usage)
	}
	if snap.Auto == nil || snap.Auto.UsedPercent != 10.5 {
		t.Fatalf("auto = %+v", snap.Auto)
	}
	if snap.Spend == nil || snap.Spend.LimitCents != 40000 || snap.Spend.RemainingCents != 16778 {
		t.Fatalf("spend = %+v", snap.Spend)
	}
	if snap.OnDemand == nil || snap.OnDemand.LimitType != "user" || snap.OnDemand.RemainingCents != 8800 {
		t.Fatalf("onDemand = %+v", snap.OnDemand)
	}
	if snap.PeriodStart == nil || snap.PeriodEnd == nil {
		t.Fatal("expected billing period timestamps")
	}
}

func TestParseCurrentPeriodUsageWithoutRemainingField(t *testing.T) {
	// Live 2026-08-18 Team account: planUsage omits `remaining`.
	body := `{
	  "billingCycleStart": "1768399334000",
	  "billingCycleEnd": "1771077734000",
	  "planUsage": {
	    "totalSpend": 1361,
	    "includedSpend": 1361,
	    "bonusSpend": 0,
	    "limit": 2000,
	    "autoPercentUsed": 12.5,
	    "apiPercentUsed": 55.5,
	    "totalPercentUsed": 68.05
	  },
	  "spendLimitUsage": {"limitType": "team", "pooledUsed": 0},
	  "displayMessage": "You've used 68% of your usage limit"
	}`
	snap, ok := parseCurrentPeriodUsage([]byte(body))
	if !ok {
		t.Fatal("live Team schema must parse")
	}
	if snap.Spend == nil || snap.Spend.LimitCents != 2000 || snap.Spend.RemainingCents != 639 {
		t.Fatalf("spend remaining should be derived from limit-included: %+v", snap.Spend)
	}
	if snap.OnDemand != nil {
		t.Fatalf("limitType-only spendLimitUsage must not invent an on-demand cap: %+v", snap.OnDemand)
	}
	if remaining, ok := snap.RemainingPercent(); !ok || remaining < 31 || remaining > 32 {
		t.Fatalf("remaining percent = %v ok=%v", remaining, ok)
	}
}

func TestParseCurrentPeriodUsageRejectsUnknownSchema(t *testing.T) {
	if _, ok := parseCurrentPeriodUsage([]byte(`{"hello":"world"}`)); ok {
		t.Fatal("unknown payload must not parse as period usage")
	}
}

func TestParseAuthUsageEnterpriseBuckets(t *testing.T) {
	buckets, ok := parseAuthUsage([]byte(fixtureEnterpriseUsage))
	if !ok {
		t.Fatal("expected enterprise request buckets")
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2: %+v", len(buckets), buckets)
	}
}

func TestApplyPlanInfoAndCredits(t *testing.T) {
	snap := QuotaSnapshot{Spend: &MoneyUsage{IncludedCents: 100}}
	applyPlanInfo(&snap, []byte(fixturePlanInfo))
	applyCreditGrants(&snap, []byte(fixtureCredits))
	if snap.Plan != "Ultra" || snap.PlanPrice != "$200/mo" {
		t.Fatalf("plan = %q price = %q", snap.Plan, snap.PlanPrice)
	}
	if snap.Spend.LimitCents != 40000 {
		t.Fatalf("included limit not copied from plan info: %+v", snap.Spend)
	}
	if snap.Credits == nil || snap.Credits.RemainingCents != 1750 || snap.Credits.PrepaidCents != 250 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
}

func TestQuotaClientFetchesAndCaches(t *testing.T) {
	var periodHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Connect-Protocol-Version"); got != "1" {
			t.Errorf("Connect-Protocol-Version = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case quotaCurrentPeriodPath:
			periodHits.Add(1)
			_, _ = io.WriteString(w, fixturePeriodUsage)
		case quotaPlanInfoPath:
			_, _ = io.WriteString(w, fixturePlanInfo)
		case quotaCreditsPath:
			_, _ = io.WriteString(w, fixtureCredits)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := NewQuotaClient()
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }

	first := client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "acct-a", AccessToken: "tok-1", AccountID: "acct-a"})
	if first.Status != QuotaStatusOK {
		t.Fatalf("first status = %s error=%+v", first.Status, first.Error)
	}
	if first.Plan != "Ultra" {
		t.Fatalf("plan = %q", first.Plan)
	}
	if remaining, ok := first.RemainingPercent(); !ok || remaining < 41 || remaining > 42 {
		t.Fatalf("remaining = %v ok=%v", remaining, ok)
	}

	now = now.Add(10 * time.Second)
	second := client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "acct-a", AccessToken: "tok-1", AccountID: "acct-a"})
	if periodHits.Load() != 1 {
		t.Fatalf("period hits = %d, want 1 (cached)", periodHits.Load())
	}
	if second.Stale {
		t.Fatal("fresh cache must not be marked stale")
	}

	now = now.Add(client.CacheTTL)
	_ = client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "acct-a", AccessToken: "tok-1", AccountID: "acct-a"})
	if periodHits.Load() != 2 {
		t.Fatalf("period hits = %d, want 2 after TTL", periodHits.Load())
	}
}

func TestQuotaClientSingleflightCoalesces(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != quotaCurrentPeriodPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if hits.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = io.WriteString(w, fixturePeriodUsage)
	}))
	t.Cleanup(srv.Close)

	client := NewQuotaClient()
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()

	done := make(chan QuotaSnapshot, 2)
	go func() {
		done <- client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "sf", AccessToken: "tok"})
	}()
	<-started
	go func() {
		done <- client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "sf", AccessToken: "tok"})
	}()
	time.Sleep(30 * time.Millisecond)
	close(release)
	a := <-done
	b := <-done
	if a.Status != QuotaStatusOK || b.Status != QuotaStatusOK {
		t.Fatalf("status a=%s b=%s", a.Status, b.Status)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 coalesced upstream call", hits.Load())
	}
}

func TestQuotaClientUnauthorizedDoesNotLookLikeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated"}}`)
	}))
	t.Cleanup(srv.Close)

	client := NewQuotaClient()
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	snap := client.Fetch(context.Background(), FetchQuotaOptions{AccessToken: "dead", AccountID: "acct"})
	if snap.Status != QuotaStatusUnavailable {
		t.Fatalf("status = %s", snap.Status)
	}
	if snap.Error == nil || snap.Error.Code != QuotaErrorUnauthorized {
		t.Fatalf("error = %+v", snap.Error)
	}
}

func TestQuotaClientReturnsStaleOnFetchError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != quotaCurrentPeriodPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if hits.Add(1) == 1 {
			_, _ = io.WriteString(w, fixturePeriodUsage)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	client := NewQuotaClient()
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	client.CacheTTL = time.Millisecond
	now := time.Now()
	client.now = func() time.Time { return now }

	ok := client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "stale", AccessToken: "tok"})
	if ok.Status != QuotaStatusOK {
		t.Fatalf("first = %+v", ok)
	}
	now = now.Add(time.Second)
	stale := client.Fetch(context.Background(), FetchQuotaOptions{CacheKey: "stale", AccessToken: "tok"})
	if !stale.Stale {
		t.Fatalf("expected stale snapshot, got %+v", stale)
	}
	if stale.Spend == nil {
		t.Fatal("stale snapshot should keep last successful spend")
	}
}

func TestQuotaClientEnterpriseFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case quotaCurrentPeriodPath:
			_, _ = io.WriteString(w, `{}`)
		case quotaAuthUsagePath:
			_, _ = io.WriteString(w, fixtureEnterpriseUsage)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client := NewQuotaClient()
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	snap := client.Fetch(context.Background(), FetchQuotaOptions{AccessToken: "tok"})
	if snap.Status != QuotaStatusOK {
		t.Fatalf("status = %s err=%+v", snap.Status, snap.Error)
	}
	if snap.Source != QuotaSourceUsageAPI || len(snap.Requests) != 2 {
		t.Fatalf("source=%s requests=%+v", snap.Source, snap.Requests)
	}
}

func TestMissingTokenIsUnauthorizedQuota(t *testing.T) {
	snap := NewQuotaClient().Fetch(context.Background(), FetchQuotaOptions{AccountID: "x"})
	if snap.Error == nil || snap.Error.Code != QuotaErrorUnauthorized {
		t.Fatalf("error = %+v", snap.Error)
	}
}
