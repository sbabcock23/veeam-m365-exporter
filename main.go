package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddress string        `yaml:"listen_address"`
	ScrapeTimeout time.Duration `yaml:"scrape_timeout"`
	Instances     []InstanceCfg `yaml:"instances"`
}

type InstanceCfg struct {
	Name               string `yaml:"name"`
	URL                string `yaml:"url"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	UsernameEnv        string `yaml:"username_env"`
	PasswordEnv        string `yaml:"password_env"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	APIVersion         string `yaml:"api_version"`
}

type APIClient struct {
	cfg       InstanceCfg
	base      string
	http      *http.Client
	mu        sync.Mutex
	access    string
	refresh   string
	expiresAt time.Time
}

type TokenResponse struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    interface{} `json:"expires_in"`
}

// FlexibleFloat64 accepts JSON numbers as well as numeric strings, which are
// both used by VB365 API responses depending on endpoint/build.
type FlexibleFloat64 float64

func (f *FlexibleFloat64) UnmarshalJSON(b []byte) error {
	v := strings.TrimSpace(string(b))
	v = strings.Trim(v, "\"")
	if v == "" || v == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return err
	}
	*f = FlexibleFloat64(n)
	return nil
}

type FlexibleInt64 int64

func (i *FlexibleInt64) UnmarshalJSON(b []byte) error {
	v := strings.TrimSpace(string(b))
	v = strings.Trim(v, "\"")
	if v == "" || v == "null" {
		*i = 0
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// Some APIs serialize integral values as "1.0".
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return err
		}
		n = int64(f)
	}
	*i = FlexibleInt64(n)
	return nil
}

type FlexibleBool bool

func (v *FlexibleBool) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, "\"")
	if s == "" || s == "null" {
		*v = false
		return nil
	}
	bv, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*v = FlexibleBool(bv)
	return nil
}

type Collection struct {
	Results []json.RawMessage `json:"results"`
	Offset  interface{}       `json:"offset"`
	Limit   interface{}       `json:"limit"`
}

type Job struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	OrganizationID string `json:"organizationId"`
	RepositoryID   string `json:"repositoryId"`
	BackupType     string `json:"backupType"`
	SchedulePolicy struct {
		ScheduleEnabled FlexibleBool `json:"scheduleEnabled"`
	} `json:"schedulePolicy"`
}

type JobSession struct {
	ID           string        `json:"id"`
	JobID        string        `json:"jobId"`
	RepositoryID string        `json:"repositoryId"`
	Details      string        `json:"details"`
	CreationTime string        `json:"creationTime"`
	EndTime      string        `json:"endTime"`
	RetryCount   FlexibleInt64 `json:"retryCount"`
	Progress     FlexibleInt64 `json:"progress"`
	JobType      string        `json:"jobType"`
	Status       string        `json:"status"`
	Statistics   struct {
		ProcessingRateBytesPS FlexibleFloat64 `json:"processingRateBytesPS"`
		ProcessingRateItemsPS FlexibleFloat64 `json:"processingRateItemsPS"`
		ReadRateBytesPS       FlexibleFloat64 `json:"readRateBytesPS"`
		WriteRateBytesPS      FlexibleFloat64 `json:"writeRateBytesPS"`
		TransferredDataBytes  FlexibleFloat64 `json:"transferredDataBytes"`
		ProcessedObjects      FlexibleFloat64 `json:"processedObjects"`
		Bottleneck            string          `json:"bottleneck"`
	} `json:"statistics"`
}

type Proxy struct {
	ID                   string          `json:"id"`
	HostName             string          `json:"hostName"`
	FQDN                 string          `json:"fqdn"`
	Status               string          `json:"status"`
	MaintenanceModeState string          `json:"maintenanceModeState"`
	CPUUsagePercent      FlexibleFloat64 `json:"cpuUsagePercent"`
	MemoryUsagePercent   FlexibleFloat64 `json:"memoryUsagePercent"`
	Version              string          `json:"version"`
}

type Repository struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ProxyID     string `json:"proxyId"`
	Type        string `json:"type"`
}

type Exporter struct {
	cfg     Config
	clients []*APIClient

	up                  *prometheus.Desc
	scrapeDuration      *prometheus.Desc
	scrapeErrors        *prometheus.Desc
	jobsTotal           *prometheus.Desc
	jobScheduled        *prometheus.Desc
	jobLastStatus       *prometheus.Desc
	jobLastProgress     *prometheus.Desc
	jobLastStart        *prometheus.Desc
	jobLastEnd          *prometheus.Desc
	jobLastDuration     *prometheus.Desc
	jobTransferredBytes *prometheus.Desc
	jobProcessedObjects *prometheus.Desc
	jobProcessingBPS    *prometheus.Desc
	jobReadBPS          *prometheus.Desc
	jobWriteBPS         *prometheus.Desc
	jobRetries          *prometheus.Desc
	repositoriesTotal   *prometheus.Desc
	repositoryInfo      *prometheus.Desc
	proxiesTotal        *prometheus.Desc
	proxyStatus         *prometheus.Desc
	proxyCPU            *prometheus.Desc
	proxyMemory         *prometheus.Desc
	proxyMaintenance    *prometheus.Desc
}

func main() {
	configPath := flag.String("config", "/etc/veeam-vb365-exporter/config.yml", "Path to YAML configuration")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	exp, err := NewExporter(cfg)
	if err != nil {
		slog.Error("exporter initialization failed", "error", err)
		os.Exit(1)
	}
	prometheus.MustRegister(exp)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "veeam-vb365-exporter\nmetrics: /metrics\nhealth: /healthz\n")
	})

	srv := &http.Server{Addr: cfg.ListenAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("starting exporter", "listen", cfg.ListenAddress, "instances", len(cfg.Instances))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	if c.ListenAddress == "" {
		c.ListenAddress = ":9810"
	}
	if c.ScrapeTimeout == 0 {
		c.ScrapeTimeout = 25 * time.Second
	}
	if len(c.Instances) == 0 {
		return Config{}, errors.New("at least one instance is required")
	}
	seen := map[string]bool{}
	for i := range c.Instances {
		v := &c.Instances[i]
		if v.UsernameEnv != "" {
			v.Username = os.Getenv(v.UsernameEnv)
		}
		if v.PasswordEnv != "" {
			v.Password = os.Getenv(v.PasswordEnv)
		}
		if v.Name == "" || v.URL == "" || v.Username == "" || v.Password == "" {
			return Config{}, fmt.Errorf("instance %d requires name, url, and credentials (directly or via environment variables)", i)
		}
		if seen[v.Name] {
			return Config{}, fmt.Errorf("duplicate instance name %q", v.Name)
		}
		seen[v.Name] = true
		if v.APIVersion == "" {
			v.APIVersion = "v8"
		}
		v.APIVersion = strings.Trim(v.APIVersion, "/")
	}
	return c, nil
}

func NewAPIClient(cfg InstanceCfg, timeout time.Duration) (*APIClient, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid URL for %s: %q", cfg.Name, cfg.URL)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}} // #nosec G402: explicitly configurable for self-signed VB365 APIs.
	return &APIClient{cfg: cfg, base: strings.TrimRight(cfg.URL, "/") + "/" + cfg.APIVersion, http: &http.Client{Timeout: timeout, Transport: tr}}, nil
}

func (c *APIClient) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access != "" && time.Now().Before(c.expiresAt.Add(-30*time.Second)) {
		return nil
	}
	if c.refresh != "" {
		if err := c.requestToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refresh}}); err == nil {
			return nil
		}
	}
	return c.requestToken(ctx, url.Values{"grant_type": {"password"}, "username": {c.cfg.Username}, "password": {c.cfg.Password}})
}

func (c *APIClient) requestToken(ctx context.Context, form url.Values) error {
	// VB365 requires an anti-forgery token on authenticated API requests by default.
	// For a non-browser Prometheus exporter, explicitly request a bearer token that
	// does not require the anti-forgery cookie/header pair.
	form.Set("disable_antiforgery_token", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token request returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var t TokenResponse
	if err := json.Unmarshal(body, &t); err != nil {
		return err
	}
	if t.AccessToken == "" {
		return errors.New("token response contained no access_token")
	}
	c.access, c.refresh = t.AccessToken, t.RefreshToken
	secs := parseNumber(t.ExpiresIn)
	if secs <= 0 {
		secs = 3600
	}
	c.expiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	return nil
}

func (c *APIClient) get(ctx context.Context, path string, out interface{}) error {
	if err := c.ensureToken(ctx); err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		c.mu.Lock()
		token := c.access
		c.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.mu.Lock()
			c.access = ""
			c.expiresAt = time.Time{}
			c.mu.Unlock()
			if err := c.ensureToken(ctx); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("GET %s returned %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
		}
		return json.Unmarshal(body, out)
	}
	return errors.New("request failed after token refresh")
}

func fetchCollection[T any](ctx context.Context, c *APIClient, endpoint string) ([]T, error) {
	const page = 10000
	var all []T
	for offset := 0; ; offset += page {
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		var coll Collection
		if err := c.get(ctx, fmt.Sprintf("%s%slimit=%d&offset=%d", endpoint, sep, page, offset), &coll); err != nil {
			return nil, err
		}
		for _, raw := range coll.Results {
			var item T
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			all = append(all, item)
		}
		if len(coll.Results) < page {
			break
		}
	}
	return all, nil
}

func NewExporter(cfg Config) (*Exporter, error) {
	e := &Exporter{cfg: cfg}
	for _, ic := range cfg.Instances {
		c, err := NewAPIClient(ic, cfg.ScrapeTimeout)
		if err != nil {
			return nil, err
		}
		e.clients = append(e.clients, c)
	}
	L := []string{"instance"}
	JL := []string{"instance", "job_id", "job_name"}
	e.up = prometheus.NewDesc("veeam_vb365_up", "Whether the VB365 instance was successfully scraped.", L, nil)
	e.scrapeDuration = prometheus.NewDesc("veeam_vb365_scrape_duration_seconds", "VB365 scrape duration.", L, nil)
	e.scrapeErrors = prometheus.NewDesc("veeam_vb365_scrape_errors", "Number of collection sections that failed during the scrape.", L, nil)
	e.jobsTotal = prometheus.NewDesc("veeam_vb365_jobs_total", "Number of configured backup jobs.", L, nil)
	e.jobScheduled = prometheus.NewDesc("veeam_vb365_job_schedule_enabled", "Whether scheduling is enabled for a backup job.", JL, nil)
	e.jobLastStatus = prometheus.NewDesc("veeam_vb365_job_last_status", "Last job session status (1 for the current status label).", append(JL, "status"), nil)
	e.jobLastProgress = prometheus.NewDesc("veeam_vb365_job_last_progress_percent", "Progress of the latest job session.", JL, nil)
	e.jobLastStart = prometheus.NewDesc("veeam_vb365_job_last_start_timestamp_seconds", "Start time of latest job session as Unix time.", JL, nil)
	e.jobLastEnd = prometheus.NewDesc("veeam_vb365_job_last_end_timestamp_seconds", "End time of latest job session as Unix time.", JL, nil)
	e.jobLastDuration = prometheus.NewDesc("veeam_vb365_job_last_duration_seconds", "Duration of latest job session.", JL, nil)
	e.jobTransferredBytes = prometheus.NewDesc("veeam_vb365_job_last_transferred_bytes", "Bytes transferred in latest job session.", JL, nil)
	e.jobProcessedObjects = prometheus.NewDesc("veeam_vb365_job_last_processed_objects", "Objects processed in latest job session.", JL, nil)
	e.jobProcessingBPS = prometheus.NewDesc("veeam_vb365_job_last_processing_rate_bytes_per_second", "Processing rate of latest job session.", JL, nil)
	e.jobReadBPS = prometheus.NewDesc("veeam_vb365_job_last_read_rate_bytes_per_second", "Read rate of latest job session.", JL, nil)
	e.jobWriteBPS = prometheus.NewDesc("veeam_vb365_job_last_write_rate_bytes_per_second", "Write rate of latest job session.", JL, nil)
	e.jobRetries = prometheus.NewDesc("veeam_vb365_job_last_retry_count", "Retry count of latest job session.", JL, nil)
	e.repositoriesTotal = prometheus.NewDesc("veeam_vb365_repositories_total", "Number of configured backup repositories.", L, nil)
	e.repositoryInfo = prometheus.NewDesc("veeam_vb365_repository_info", "Backup repository metadata.", []string{"instance", "repository_id", "repository_name", "repository_type"}, nil)
	e.proxiesTotal = prometheus.NewDesc("veeam_vb365_proxies_total", "Number of configured backup proxies.", L, nil)
	e.proxyStatus = prometheus.NewDesc("veeam_vb365_proxy_status", "Backup proxy status (1 for current status label).", []string{"instance", "proxy_id", "proxy_name", "status", "version"}, nil)
	e.proxyCPU = prometheus.NewDesc("veeam_vb365_proxy_cpu_usage_percent", "Backup proxy CPU usage percentage.", []string{"instance", "proxy_id", "proxy_name"}, nil)
	e.proxyMemory = prometheus.NewDesc("veeam_vb365_proxy_memory_usage_percent", "Backup proxy memory usage percentage.", []string{"instance", "proxy_id", "proxy_name"}, nil)
	e.proxyMaintenance = prometheus.NewDesc("veeam_vb365_proxy_maintenance_mode", "Backup proxy maintenance mode (1 when enabled-like state, else 0).", []string{"instance", "proxy_id", "proxy_name", "state"}, nil)
	return e, nil
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{e.up, e.scrapeDuration, e.scrapeErrors, e.jobsTotal, e.jobScheduled, e.jobLastStatus, e.jobLastProgress, e.jobLastStart, e.jobLastEnd, e.jobLastDuration, e.jobTransferredBytes, e.jobProcessedObjects, e.jobProcessingBPS, e.jobReadBPS, e.jobWriteBPS, e.jobRetries, e.repositoriesTotal, e.repositoryInfo, e.proxiesTotal, e.proxyStatus, e.proxyCPU, e.proxyMemory, e.proxyMaintenance} {
		ch <- d
	}
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	for _, c := range e.clients {
		wg.Add(1)
		go func(c *APIClient) { defer wg.Done(); e.collectInstance(c, ch) }(c)
	}
	wg.Wait()
}

func (e *Exporter) collectInstance(c *APIClient, ch chan<- prometheus.Metric) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ScrapeTimeout)
	defer cancel()
	instance := c.cfg.Name
	failures := 0

	jobs, err := fetchCollection[Job](ctx, c, "/Jobs")
	if err != nil {
		failures++
		slog.Warn("jobs collection failed", "instance", instance, "error", err)
	} else {
		ch <- prometheus.MustNewConstMetric(e.jobsTotal, prometheus.GaugeValue, float64(len(jobs)), instance)
		jobNames := map[string]string{}
		for _, j := range jobs {
			name := firstNonEmpty(j.Name, j.Description, j.ID)
			jobNames[j.ID] = name
			enabled := 0.0
			if bool(j.SchedulePolicy.ScheduleEnabled) {
				enabled = 1
			}
			ch <- prometheus.MustNewConstMetric(e.jobScheduled, prometheus.GaugeValue, enabled, instance, j.ID, name)
		}
		sessions, serr := fetchCollection[JobSession](ctx, c, "/JobSessions")
		if serr != nil {
			failures++
			slog.Warn("job sessions collection failed", "instance", instance, "error", serr)
		} else {
			e.emitLatestSessions(instance, jobNames, sessions, ch)
		}
	}

	repos, err := fetchCollection[Repository](ctx, c, "/BackupRepositories")
	if err != nil {
		failures++
		slog.Warn("repositories collection failed", "instance", instance, "error", err)
	} else {
		ch <- prometheus.MustNewConstMetric(e.repositoriesTotal, prometheus.GaugeValue, float64(len(repos)), instance)
		for _, r := range repos {
			ch <- prometheus.MustNewConstMetric(e.repositoryInfo, prometheus.GaugeValue, 1, instance, r.ID, firstNonEmpty(r.Name, r.Description, r.ID), r.Type)
		}
	}

	proxies, err := fetchCollection[Proxy](ctx, c, "/Proxies")
	if err != nil {
		failures++
		slog.Warn("proxies collection failed", "instance", instance, "error", err)
	} else {
		ch <- prometheus.MustNewConstMetric(e.proxiesTotal, prometheus.GaugeValue, float64(len(proxies)), instance)
		for _, p := range proxies {
			name := firstNonEmpty(p.FQDN, p.HostName, p.ID)
			ch <- prometheus.MustNewConstMetric(e.proxyStatus, prometheus.GaugeValue, 1, instance, p.ID, name, p.Status, p.Version)
			ch <- prometheus.MustNewConstMetric(e.proxyCPU, prometheus.GaugeValue, float64(p.CPUUsagePercent), instance, p.ID, name)
			ch <- prometheus.MustNewConstMetric(e.proxyMemory, prometheus.GaugeValue, float64(p.MemoryUsagePercent), instance, p.ID, name)
			m := 0.0
			if !strings.EqualFold(p.MaintenanceModeState, "Disabled") && p.MaintenanceModeState != "" {
				m = 1
			}
			ch <- prometheus.MustNewConstMetric(e.proxyMaintenance, prometheus.GaugeValue, m, instance, p.ID, name, p.MaintenanceModeState)
		}
	}

	up := 1.0
	if failures > 0 {
		up = 0
	}
	ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, up, instance)
	ch <- prometheus.MustNewConstMetric(e.scrapeErrors, prometheus.GaugeValue, float64(failures), instance)
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds(), instance)
}

func (e *Exporter) emitLatestSessions(instance string, jobNames map[string]string, sessions []JobSession, ch chan<- prometheus.Metric) {
	latest := map[string]JobSession{}
	for _, s := range sessions {
		old, ok := latest[s.JobID]
		if !ok || sessionTime(s).After(sessionTime(old)) {
			latest[s.JobID] = s
		}
	}
	for jobID, s := range latest {
		name := firstNonEmpty(jobNames[jobID], jobID)
		labels := []string{instance, jobID, name}
		ch <- prometheus.MustNewConstMetric(e.jobLastStatus, prometheus.GaugeValue, 1, append(labels, s.Status)...)
		ch <- prometheus.MustNewConstMetric(e.jobLastProgress, prometheus.GaugeValue, float64(s.Progress), labels...)
		start, sok := parseTime(s.CreationTime)
		end, eok := parseTime(s.EndTime)
		if sok {
			ch <- prometheus.MustNewConstMetric(e.jobLastStart, prometheus.GaugeValue, float64(start.Unix()), labels...)
		}
		if eok {
			ch <- prometheus.MustNewConstMetric(e.jobLastEnd, prometheus.GaugeValue, float64(end.Unix()), labels...)
		}
		if sok && eok {
			ch <- prometheus.MustNewConstMetric(e.jobLastDuration, prometheus.GaugeValue, end.Sub(start).Seconds(), labels...)
		}
		ch <- prometheus.MustNewConstMetric(e.jobTransferredBytes, prometheus.GaugeValue, float64(s.Statistics.TransferredDataBytes), labels...)
		ch <- prometheus.MustNewConstMetric(e.jobProcessedObjects, prometheus.GaugeValue, float64(s.Statistics.ProcessedObjects), labels...)
		ch <- prometheus.MustNewConstMetric(e.jobProcessingBPS, prometheus.GaugeValue, float64(s.Statistics.ProcessingRateBytesPS), labels...)
		ch <- prometheus.MustNewConstMetric(e.jobReadBPS, prometheus.GaugeValue, float64(s.Statistics.ReadRateBytesPS), labels...)
		ch <- prometheus.MustNewConstMetric(e.jobWriteBPS, prometheus.GaugeValue, float64(s.Statistics.WriteRateBytesPS), labels...)
		ch <- prometheus.MustNewConstMetric(e.jobRetries, prometheus.GaugeValue, float64(s.RetryCount), labels...)
	}
}

func sessionTime(s JobSession) time.Time {
	if t, ok := parseTime(s.EndTime); ok {
		return t
	}
	if t, ok := parseTime(s.CreationTime); ok {
		return t
	}
	return time.Time{}
}
func parseTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	return t, err == nil
}
func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "unknown"
}
func parseNumber(v interface{}) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}
