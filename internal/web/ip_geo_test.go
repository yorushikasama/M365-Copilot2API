package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func resetIPGeoCache(t *testing.T) {
	t.Helper()
	ipGeoCache.mu.Lock()
	ipGeoCache.m = map[string]ipGeoEntry{}
	ipGeoCache.mu.Unlock()
}

func TestResolveIPIncludesProviderGeolocation(t *testing.T) {
	resetIPGeoCache(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","country":"Japan","countryCode":"JP","regionName":"Tokyo","city":"Chiyoda","isp":"Example ISP","as":"AS64500 Example","timezone":"Asia/Tokyo"}`))
	}))
	defer srv.Close()
	t.Setenv("M365_IP_GEO_URL", srv.URL+"/json/{ip}")

	res, err := resolveIP(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	if res.Geo == nil {
		t.Fatal("expected geolocation in resolution")
	}
	if res.Geo.City != "Chiyoda" || res.Geo.Region != "Tokyo" || res.Geo.Country != "Japan" || res.Geo.CountryCode != "JP" {
		t.Fatalf("geo=%+v", res.Geo)
	}
	if res.Geo.ISP != "Example ISP" || res.Geo.ASN != "AS64500 Example" || res.Geo.Source == "" {
		t.Fatalf("geo=%+v", res.Geo)
	}

	// A second resolution of the same address must be served from the cache so
	// the rate-limited free provider is not queried once per page refresh.
	if _, err := resolveIP(context.Background(), "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("provider called %d times, want 1", got)
	}

	// Private addresses stay local: no client IP is sent to the provider.
	res, err = resolveIP(context.Background(), "192.168.1.10")
	if err != nil {
		t.Fatal(err)
	}
	if res.Geo != nil {
		t.Fatalf("private address leaked to provider: %+v", res.Geo)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("provider called %d times after private lookup, want 1", got)
	}
}

func TestLookupIPGeoDisabledAndFailuresCached(t *testing.T) {
	resetIPGeoCache(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"fail","message":"reserved range"}`))
	}))
	defer srv.Close()

	t.Setenv("M365_IP_GEO_URL", srv.URL+"/json/{ip}")
	t.Setenv("M365_IP_GEO_DISABLE", "1")
	if geo := lookupIPGeo(context.Background(), "198.51.100.9"); geo != nil {
		t.Fatalf("lookup ran while disabled: %+v", geo)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("provider called %d times while disabled", got)
	}

	t.Setenv("M365_IP_GEO_DISABLE", "")
	if geo := lookupIPGeo(context.Background(), "198.51.100.9"); geo != nil {
		t.Fatalf("failed lookup returned %+v", geo)
	}
	if geo := lookupIPGeo(context.Background(), "198.51.100.9"); geo != nil {
		t.Fatalf("failed lookup returned %+v", geo)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("provider called %d times, want 1 negative-cached call", got)
	}
}

func TestIPGeoResponseMapsIPInfoFields(t *testing.T) {
	geo := ipGeoResponse{Country: "us", Region: "California", City: "Mountain View", Org: "AS15169 Google LLC", Timezone: "America/Los_Angeles"}.toGeo()
	if geo.CountryCode != "US" || geo.Region != "California" || geo.ISP != "AS15169 Google LLC" {
		t.Fatalf("geo=%+v", geo)
	}
	if geo.empty() {
		t.Fatal("mapped response reported as empty")
	}
	if !(ipGeoResponse{}).toGeo().empty() {
		t.Fatal("blank response reported as non-empty")
	}
}

func TestIPGeoRequestURLSupportsBaseAndTemplate(t *testing.T) {
	if got := ipGeoRequestURL("https://example.test/json/{ip}?x=1", "1.2.3.4"); got != "https://example.test/json/1.2.3.4?x=1" {
		t.Fatalf("template url=%q", got)
	}
	if got := ipGeoRequestURL("https://ipinfo.io/", "1.2.3.4"); got != "https://ipinfo.io/1.2.3.4" {
		t.Fatalf("base url=%q", got)
	}
}
