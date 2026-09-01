package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// IPGeo holds the geolocation attributes returned by the configured lookup
// provider. Every field is optional because providers differ in coverage.
type IPGeo struct {
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"countryCode,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
	ISP         string `json:"isp,omitempty"`
	ASN         string `json:"asn,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	Source      string `json:"source,omitempty"`
}

// defaultIPGeoURL is the free ip-api.com endpoint. The free tier is HTTP only
// and rate limited to roughly 45 requests per minute per source address.
const defaultIPGeoURL = "http://ip-api.com/json/{ip}?fields=status,message,country,countryCode,regionName,city,isp,org,as,timezone"

const (
	ipGeoTTL         = 12 * time.Hour
	ipGeoNegativeTTL = 5 * time.Minute
	ipGeoCacheMax    = 2048
)

var ipGeoHTTPClient = &http.Client{Timeout: 6 * time.Second}

type ipGeoEntry struct {
	geo     *IPGeo
	expires time.Time
}

var ipGeoCache = struct {
	mu sync.Mutex
	m  map[string]ipGeoEntry
}{m: map[string]ipGeoEntry{}}

// ipGeoEndpoint returns the lookup URL template, or "" when lookups are off.
// M365_IP_GEO_DISABLE turns the third-party call off entirely; M365_IP_GEO_URL
// swaps in another provider and may contain a {ip} placeholder.
func ipGeoEndpoint() string {
	if strings.TrimSpace(os.Getenv("M365_IP_GEO_DISABLE")) != "" {
		return ""
	}
	if v := strings.TrimSpace(os.Getenv("M365_IP_GEO_URL")); v != "" {
		return v
	}
	return defaultIPGeoURL
}

// ipGeoResponse covers the field names used by ip-api.com and ipinfo.io so one
// decoder handles either provider.
type ipGeoResponse struct {
	Status      string `json:"status"`
	Message     string `json:"message"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	RegionName  string `json:"regionName"`
	Region      string `json:"region"`
	City        string `json:"city"`
	ISP         string `json:"isp"`
	Org         string `json:"org"`
	AS          string `json:"as"`
	ASN         string `json:"asn"`
	Timezone    string `json:"timezone"`
}

func (r ipGeoResponse) toGeo() IPGeo {
	geo := IPGeo{
		Country:     strings.TrimSpace(r.Country),
		CountryCode: strings.TrimSpace(r.CountryCode),
		Region:      strings.TrimSpace(r.RegionName),
		City:        strings.TrimSpace(r.City),
		ISP:         strings.TrimSpace(r.ISP),
		ASN:         strings.TrimSpace(r.AS),
		Timezone:    strings.TrimSpace(r.Timezone),
	}
	if geo.Region == "" {
		geo.Region = strings.TrimSpace(r.Region)
	}
	if geo.ISP == "" {
		geo.ISP = strings.TrimSpace(r.Org)
	}
	if geo.ASN == "" {
		geo.ASN = strings.TrimSpace(r.ASN)
	}
	// ipinfo.io reports the country as a two-letter code instead of a name.
	if geo.CountryCode == "" && len(geo.Country) == 2 {
		geo.CountryCode = strings.ToUpper(geo.Country)
	}
	return geo
}

func (g IPGeo) empty() bool {
	return g.Country == "" && g.CountryCode == "" && g.Region == "" && g.City == "" && g.ISP == "" && g.ASN == ""
}

func ipGeoRequestURL(endpoint, ip string) string {
	if strings.Contains(endpoint, "{ip}") {
		return strings.ReplaceAll(endpoint, "{ip}", url.PathEscape(ip))
	}
	return strings.TrimSuffix(endpoint, "/") + "/" + url.PathEscape(ip)
}

func ipGeoCacheGet(ip string) (*IPGeo, bool) {
	ipGeoCache.mu.Lock()
	defer ipGeoCache.mu.Unlock()
	entry, ok := ipGeoCache.m[ip]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.geo, true
}

func ipGeoCacheSet(ip string, geo *IPGeo, ttl time.Duration) {
	ipGeoCache.mu.Lock()
	defer ipGeoCache.mu.Unlock()
	if len(ipGeoCache.m) >= ipGeoCacheMax {
		now := time.Now()
		for k, v := range ipGeoCache.m {
			if now.After(v.expires) {
				delete(ipGeoCache.m, k)
			}
		}
		for k := range ipGeoCache.m {
			if len(ipGeoCache.m) < ipGeoCacheMax {
				break
			}
			delete(ipGeoCache.m, k)
		}
	}
	ipGeoCache.m[ip] = ipGeoEntry{geo: geo, expires: time.Now().Add(ttl)}
}

// lookupIPGeo asks the configured third-party provider where a public IP is.
// Failures are cached briefly and reported as nil so the caller keeps serving
// the locally computed part of the resolution.
func lookupIPGeo(ctx context.Context, ip string) *IPGeo {
	endpoint := ipGeoEndpoint()
	if endpoint == "" {
		return nil
	}
	if geo, ok := ipGeoCacheGet(ip); ok {
		return geo
	}
	reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ipGeoRequestURL(endpoint, ip), nil)
	if err != nil {
		ipGeoCacheSet(ip, nil, ipGeoNegativeTTL)
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := ipGeoHTTPClient.Do(req)
	if err != nil {
		log.Printf("[ip-geo] lookup failed: %v", err)
		ipGeoCacheSet(ip, nil, ipGeoNegativeTTL)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[ip-geo] provider returned status %d", resp.StatusCode)
		ipGeoCacheSet(ip, nil, ipGeoNegativeTTL)
		return nil
	}
	var body ipGeoResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&body); err != nil {
		log.Printf("[ip-geo] could not decode provider response: %v", err)
		ipGeoCacheSet(ip, nil, ipGeoNegativeTTL)
		return nil
	}
	if strings.EqualFold(body.Status, "fail") {
		ipGeoCacheSet(ip, nil, ipGeoNegativeTTL)
		return nil
	}
	geo := body.toGeo()
	if geo.empty() {
		ipGeoCacheSet(ip, nil, ipGeoNegativeTTL)
		return nil
	}
	if u, err := url.Parse(ipGeoRequestURL(endpoint, ip)); err == nil {
		geo.Source = u.Host
	}
	ipGeoCacheSet(ip, &geo, ipGeoTTL)
	return &geo
}
