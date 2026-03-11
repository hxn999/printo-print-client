package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Injected at build time by -ldflags "-X main.version=vX.Y.Z"
var version = "dev"

// ─────────────────────────────────────────────
// Models
// ─────────────────────────────────────────────

type Print struct {
	ID                    int    `json:"id"`
	Name                  string `json:"name"`
	FileURL               string `json:"file_url"`
	Copies                int    `json:"copies"`
	Sides                 string `json:"sides"`
	PrintColor            string `json:"print_color"`
	PrintPages            string `json:"print_pages"`
	PageRange             string `json:"page_range"`
	PagesPerSlide         int    `json:"pages_per_slide"`
	TotalPages            int    `json:"total_pages"`
	RemainingPrintOptions int    `json:"remaining_printing_options"`
	Cost                  int    `json:"cost"`
}

type Order struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	TotalCost int     `json:"total_cost"`
	Prints    []Print `json:"prints"`
}

// ─────────────────────────────────────────────
// In-memory state
// ─────────────────────────────────────────────

type PrintJobTrack struct {
	CupsJobID int
	PrintID   int
	OrderID   int
	Done      bool
	Success   bool
}

type AppState struct {
	mu              sync.Mutex
	submittedPrints map[int]bool
	activeJobs      map[int]*PrintJobTrack
	orderPending    map[int]map[int]bool
	completedOrders map[int]bool

	// orderPrints tracks which file paths belong to which order,
	// so we can delete them from cache once the order is PRINTED.
	orderPrints map[int][]string // orderID → []filePath

	cacheDir string
	client   *http.Client
	baseURL  string
}

func NewAppState(client *http.Client, baseURL, cacheDir string) *AppState {
	return &AppState{
		submittedPrints: make(map[int]bool),
		activeJobs:      make(map[int]*PrintJobTrack),
		orderPending:    make(map[int]map[int]bool),
		completedOrders: make(map[int]bool),
		orderPrints:     make(map[int][]string),
		cacheDir:        cacheDir,
		client:          client,
		baseURL:         baseURL,
	}
}

// ─────────────────────────────────────────────
// Logger (stdout + /var/log/printo/client.log)
// — if run standalone (not via updater), it
//   initialises its own log file.
// ─────────────────────────────────────────────

const (
	logDir      = "/var/log/printo"
	logFile     = logDir + "/client.log"
	maxLogBytes = 5 * 1024 * 1024 // 5 MB
)

var logger *log.Logger

func initLogger() {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		logger = log.New(os.Stdout, "", 0)
		return
	}

	rotateIfNeeded(logFile)

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger = log.New(os.Stdout, "", 0)
		return
	}

	logger = log.New(io.MultiWriter(os.Stdout, f), "", 0)
}

func rotateIfNeeded(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxLogBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

func logf(format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	logger.Printf("[client "+ts+"] "+format, args...)
}

// ─────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────

func main() {
	initLogger()

	cacheDir := filepath.Join(os.Getenv("HOME"), ".print_cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		logf("FATAL: cannot create cache dir: %v\n", err)
		os.Exit(1)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	baseURL := "https://api.printobd.com"

	loginData, _ := json.Marshal(map[string]string{
		"username": "hxn",
		"password": "brimjett",
	})
	resp, err := client.Post(baseURL+"/api/accounts/auth/login/", "application/json", bytes.NewBuffer(loginData))
	if err != nil || resp.StatusCode != 200 {
		logf("FATAL: login failed: %v\n", err)
		os.Exit(1)
	}
	resp.Body.Close()

	logf("logged in — version=%s cache=%s log=%s\n", version, cacheDir, logFile)
	logf("polling every 10s...\n\n")

	state := NewAppState(client, baseURL, cacheDir)

	runCycle(state)
	for range time.NewTicker(10 * time.Second).C {
		runCycle(state)
	}
}

// ─────────────────────────────────────────────
// One poll cycle
// ─────────────────────────────────────────────

func runCycle(s *AppState) {
	ts := time.Now().Format("15:04:05")
	logf("── %s ──────────────────────────────\n", ts)

	checkCupsJobs(s)

	resp, err := s.client.Get(s.baseURL + "/api/prints/orders/my_orders/")
	if err != nil {
		logf("fetch error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var orders []Order
	if err := json.Unmarshal(body, &orders); err != nil {
		logf("JSON error: %v\n", err)
		return
	}
	logf("%d order(s) received\n", len(orders))

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range orders {
		order := &orders[i]

		if s.completedOrders[order.ID] || order.Status == "PRINTED" {
			s.completedOrders[order.ID] = true
			continue
		}

		for _, p := range order.Prints {
			if p.FileURL == "" || s.submittedPrints[p.ID] {
				continue
			}

			filePath, err := ensureDownloaded(p, s.cacheDir)
			if err != nil {
				logf("[Order %d] ✗ download '%s': %v\n", order.ID, p.Name, err)
				continue
			}

			cupsJobID, err := submitToCups(p, filePath)
			if err != nil {
				logf("[Order %d] ✗ CUPS '%s': %v\n", order.ID, p.Name, err)
				continue
			}

			logf("[Order %d] ✓ '%s' → CUPS job #%d\n", order.ID, p.Name, cupsJobID)

			s.submittedPrints[p.ID] = true
			s.activeJobs[cupsJobID] = &PrintJobTrack{
				CupsJobID: cupsJobID,
				PrintID:   p.ID,
				OrderID:   order.ID,
			}
			if s.orderPending[order.ID] == nil {
				s.orderPending[order.ID] = make(map[int]bool)
			}
			s.orderPending[order.ID][p.ID] = true

			// Record the file path for later cache cleanup.
			s.orderPrints[order.ID] = append(s.orderPrints[order.ID], filePath)
		}
	}
}

// ─────────────────────────────────────────────
// Download with local cache
// ─────────────────────────────────────────────

func ensureDownloaded(p Print, cacheDir string) (string, error) {
	filename := fmt.Sprintf("%d_%s", p.ID, sanitize(p.Name))
	filePath := filepath.Join(cacheDir, filename)

	if _, err := os.Stat(filePath); err == nil {
		logf("  cache hit: %s\n", filename)
		return filePath, nil
	}

	logf("  downloading: %s\n", p.Name)

	resp, err := http.Get(p.FileURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	tmp := filePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	f.Close()
	return filePath, os.Rename(tmp, filePath)
}

// ─────────────────────────────────────────────
// CUPS submission
// ─────────────────────────────────────────────

func submitToCups(p Print, filePath string) (int, error) {
	args := []string{"-n", strconv.Itoa(max1(p.Copies))}

	switch p.Sides {
	case "DOUBLE_SIDED":
		args = append(args, "-o", "sides=two-sided-long-edge")
	default:
		args = append(args, "-o", "sides=one-sided")
	}

	switch p.PrintColor {
	case "COLOR":
		args = append(args, "-o", "print-color-mode=color")
	default:
		args = append(args, "-o", "print-color-mode=monochrome")
	}

	if p.PrintPages == "CUSTOM" && strings.TrimSpace(p.PageRange) != "" {
		args = append(args, "-o", "page-ranges="+strings.TrimSpace(p.PageRange))
	}

	if p.PagesPerSlide > 1 {
		args = append(args, "-o", fmt.Sprintf("number-up=%d", p.PagesPerSlide))
		args = append(args, "-o", "number-up-layout=lrtb")
	}

	args = append(args, "-t", fmt.Sprintf("[PrintID-%d] %s", p.ID, p.Name))
	args = append(args, filePath)

	out, err := exec.Command("lp", args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%v — %s", err, strings.TrimSpace(string(out)))
	}

	jobID := parseCupsJobID(string(out))
	if jobID == 0 {
		return 0, fmt.Errorf("could not parse CUPS job ID from: %s", strings.TrimSpace(string(out)))
	}
	return jobID, nil
}

func parseCupsJobID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "is" && i+1 < len(fields) {
				token := fields[i+1]
				if idx := strings.LastIndex(token, "-"); idx >= 0 {
					if id, err := strconv.Atoi(token[idx+1:]); err == nil {
						return id
					}
				}
			}
		}
	}
	return 0
}

// ─────────────────────────────────────────────
// CUPS job status check
// ─────────────────────────────────────────────

func checkCupsJobs(s *AppState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeJobs) == 0 {
		return
	}

	completed := cupsCompletedJobIDs()
	failed := cupsFailedJobIDs()

	readyOrders := map[int]bool{}

	for cupsID, job := range s.activeJobs {
		if job.Done {
			continue
		}

		switch {
		case completed[cupsID]:
			logf("✓ CUPS #%d done    (print ID %d, order %d)\n", cupsID, job.PrintID, job.OrderID)
			job.Done, job.Success = true, true
			delete(s.orderPending[job.OrderID], job.PrintID)
			readyOrders[job.OrderID] = true

		case failed[cupsID]:
			logf("✗ CUPS #%d FAILED  (print ID %d, order %d)\n", cupsID, job.PrintID, job.OrderID)
			job.Done, job.Success = true, false
			delete(s.orderPending[job.OrderID], job.PrintID)
			readyOrders[job.OrderID] = true
		}
	}

	for orderID := range readyOrders {
		if pending, ok := s.orderPending[orderID]; ok && len(pending) == 0 {
			go markOrderPrinted(s, orderID)
		}
	}
}

func cupsCompletedJobIDs() map[int]bool {
	ids := map[int]bool{}
	out, err := exec.Command("lpstat", "-W", "completed").CombinedOutput()
	if err != nil {
		return ids
	}
	for _, line := range strings.Split(string(out), "\n") {
		if id := jobIDFromLpstatLine(line); id > 0 {
			ids[id] = true
		}
	}
	return ids
}

func cupsFailedJobIDs() map[int]bool {
	ids := map[int]bool{}
	out, err := exec.Command("lpstat").CombinedOutput()
	if err != nil {
		return ids
	}
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "aborted") || strings.Contains(lower, "canceled") {
			if id := jobIDFromLpstatLine(line); id > 0 {
				ids[id] = true
			}
		}
	}
	return ids
}

func jobIDFromLpstatLine(line string) int {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	token := fields[0]
	if idx := strings.LastIndex(token, "-"); idx >= 0 {
		if id, err := strconv.Atoi(token[idx+1:]); err == nil {
			return id
		}
	}
	return 0
}

// ─────────────────────────────────────────────
// Mark order PRINTED + delete cache files
// ─────────────────────────────────────────────

func markOrderPrinted(s *AppState, orderID int) {
	payload, _ := json.Marshal(map[string]interface{}{
		"order_ids": []int{orderID},
		"status":    "PRINTED",
	})

	resp, err := s.client.Post(
		s.baseURL+"/api/prints/orders/bulk_update_status/",
		"application/json",
		bytes.NewBuffer(payload),
	)
	if err != nil {
		logf("[Order %d] bulk_update_status error: %v\n", orderID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		logf("[Order %d] ✓ marked PRINTED on server\n", orderID)

		s.mu.Lock()
		s.completedOrders[orderID] = true
		filePaths := s.orderPrints[orderID]
		delete(s.orderPrints, orderID)
		s.mu.Unlock()

		// Delete cached files now that the order is confirmed PRINTED.
		deleteCacheFiles(orderID, filePaths)
	} else {
		b, _ := io.ReadAll(resp.Body)
		logf("[Order %d] bulk_update_status HTTP %d: %s\n", orderID, resp.StatusCode, string(b))
	}
}

func deleteCacheFiles(orderID int, paths []string) {
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			if !os.IsNotExist(err) {
				logf("[Order %d] ✗ delete cache '%s': %v\n", orderID, p, err)
			}
		} else {
			logf("[Order %d] ✓ deleted cache: %s\n", orderID, filepath.Base(p))
		}
	}
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func sanitize(name string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", `"`, "_", "<", "_", ">", "_", "|", "_", " ", "_",
	)
	return r.Replace(name)
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}