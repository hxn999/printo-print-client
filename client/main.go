package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// ─────────────────────────────────────────────
// Models (mirrors Django Print / PrintOrder)
// ─────────────────────────────────────────────

type Print struct {
	ID                    int    `json:"id"`
	Name                  string `json:"name"`
	FileURL               string `json:"file_url"`
	Copies                int    `json:"copies"`
	Sides                 string `json:"sides"`           // SINGLE_SIDED | DOUBLE_SIDED
	PrintColor            string `json:"print_color"`     // B_W | COLOR
	PrintPages            string `json:"print_pages"`     // ALL | CUSTOM
	PageRange             string `json:"page_range"`
	PagesPerSlide         int    `json:"pages_per_slide"` // 1 | 2 | 4 | 8 | 16
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
// In-memory state (all access guarded by mu)
// ─────────────────────────────────────────────

type PrintJobTrack struct {
	CupsJobID int
	PrintID   int
	OrderID   int
	Done      bool
	Success   bool
}

type AppState struct {
	mu sync.Mutex

	// Print IDs already submitted to CUPS (keyed by print.ID)
	submittedPrints map[int]bool

	// CUPS job ID → tracking info
	activeJobs map[int]*PrintJobTrack

	// Order ID → set of PrintIDs still waiting on CUPS
	orderPending map[int]map[int]bool

	// Order IDs already marked PRINTED on the server
	completedOrders map[int]bool

	// Local cache directory for downloaded files
	cacheDir string

	// HTTP client (shared, thread-safe)
	client  *http.Client
	baseURL string
}

func NewAppState(client *http.Client, baseURL, cacheDir string) *AppState {
	return &AppState{
		submittedPrints: make(map[int]bool),
		activeJobs:      make(map[int]*PrintJobTrack),
		orderPending:    make(map[int]map[int]bool),
		completedOrders: make(map[int]bool),
		cacheDir:        cacheDir,
		client:          client,
		baseURL:         baseURL,
	}
}

// ─────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────

func main() {
	cacheDir := filepath.Join(os.Getenv("HOME"), ".print_cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "Cannot create cache dir:", err)
		os.Exit(1)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	baseURL := "https://api.printobd.com"

	// ── Login ──────────────────────────────────
	loginData, _ := json.Marshal(map[string]string{
		"username": "hxn",
		"password": "brimjett",
	})
	resp, err := client.Post(baseURL+"/api/accounts/auth/login/", "application/json", bytes.NewBuffer(loginData))
	if err != nil || resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "Login failed:", err)
		os.Exit(1)
	}
	resp.Body.Close()
	fmt.Printf("✓ Logged in\n✓ Cache dir: %s\n✓ Polling every 10 s...\n\n", cacheDir)

	state := NewAppState(client, baseURL, cacheDir)

	// Run immediately, then every 10 s
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
	fmt.Printf("── %s ──────────────────────────────\n", ts)

	// Step 1: check existing CUPS jobs → possibly mark orders PRINTED
	checkCupsJobs(s)

	// Step 2: fetch orders from server
	resp, err := s.client.Get(s.baseURL + "/api/prints/orders/my_orders/")
	if err != nil {
		fmt.Println("  Fetch error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var orders []Order
	if err := json.Unmarshal(body, &orders); err != nil {
		fmt.Println("  JSON error:", err)
		return
	}
	fmt.Printf("  %d order(s) received\n", len(orders))

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

			// Download (cached)
			filePath, err := ensureDownloaded(p, s.cacheDir)
			if err != nil {
				fmt.Printf("  [Order %d] ✗ Download '%s': %v\n", order.ID, p.Name, err)
				continue
			}

			// Submit to CUPS
			cupsJobID, err := submitToCups(p, filePath)
			if err != nil {
				fmt.Printf("  [Order %d] ✗ CUPS '%s': %v\n", order.ID, p.Name, err)
				continue
			}

			fmt.Printf("  [Order %d] ✓ '%s' → CUPS job #%d\n", order.ID, p.Name, cupsJobID)

			// Track state
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
		}
	}
}

// ─────────────────────────────────────────────
// Download with local cache
// ─────────────────────────────────────────────

func ensureDownloaded(p Print, cacheDir string) (string, error) {
	// Unique filename: <printID>_<sanitized original name>
	filename := fmt.Sprintf("%d_%s", p.ID, sanitize(p.Name))
	filePath := filepath.Join(cacheDir, filename)

	if _, err := os.Stat(filePath); err == nil {
		fmt.Printf("    → cache hit: %s\n", filename)
		return filePath, nil
	}

	fmt.Printf("    → downloading: %s\n", p.Name)

	resp, err := http.Get(p.FileURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Write to .tmp then rename (atomic)
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
// CUPS submission via lp
// ─────────────────────────────────────────────

func submitToCups(p Print, filePath string) (int, error) {
	// args := []string{"-d", "default"}

	// ── Copies ────────────────────────────────
	args := []string{ "-n", strconv.Itoa(max1(p.Copies))}

	// ── Sides ─────────────────────────────────
	switch p.Sides {
	case "DOUBLE_SIDED":
		args = append(args, "-o", "sides=two-sided-long-edge")
	default:
		args = append(args, "-o", "sides=one-sided")
	}

	// ── Color ─────────────────────────────────
	switch p.PrintColor {
	case "COLOR":
		args = append(args, "-o", "print-color-mode=color")
	default:
		args = append(args, "-o", "print-color-mode=monochrome")
	}

	// ── Page range ────────────────────────────
	if p.PrintPages == "CUSTOM" && strings.TrimSpace(p.PageRange) != "" {
		args = append(args, "-o", "page-ranges="+strings.TrimSpace(p.PageRange))
	}

	// ── N-up (pages per slide) ────────────────
	if p.PagesPerSlide > 1 {
		args = append(args, "-o", fmt.Sprintf("number-up=%d", p.PagesPerSlide))
		// Natural reading order
		args = append(args, "-o", "number-up-layout=lrtb")
	}

	// ── Job title ─────────────────────────────
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

// "request id is <printer>-42 (1 file(s))"
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
			fmt.Printf("  ✓ CUPS #%d done    (print ID %d, order %d)\n", cupsID, job.PrintID, job.OrderID)
			job.Done, job.Success = true, true
			delete(s.orderPending[job.OrderID], job.PrintID)
			readyOrders[job.OrderID] = true

		case failed[cupsID]:
			fmt.Printf("  ✗ CUPS #%d FAILED  (print ID %d, order %d)\n", cupsID, job.PrintID, job.OrderID)
			job.Done, job.Success = true, false
			// Remove so we don't block the order indefinitely
			delete(s.orderPending[job.OrderID], job.PrintID)
			readyOrders[job.OrderID] = true
		}
	}

	// Fire bulk_update_status for orders where every print is resolved
	for orderID := range readyOrders {
		if pending, ok := s.orderPending[orderID]; ok && len(pending) == 0 {
			go markOrderPrinted(s, orderID)
		}
	}
}

// lpstat -W completed  →  lines: "printer-42  user  size  date  title"
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

// Aborted / canceled jobs come back in lpstat without a -W flag (active queue)
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
	token := fields[0] // e.g. "HP_LaserJet-42"
	if idx := strings.LastIndex(token, "-"); idx >= 0 {
		if id, err := strconv.Atoi(token[idx+1:]); err == nil {
			return id
		}
	}
	return 0
}

// ─────────────────────────────────────────────
// Bulk status update  POST /api/prints/print-orders/bulk_update_status/
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
		fmt.Printf("  [Order %d] bulk_update_status error: %v\n", orderID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		fmt.Printf("  [Order %d] ✓ marked PRINTED on server\n", orderID)
		s.mu.Lock()
		s.completedOrders[orderID] = true
		s.mu.Unlock()
	} else {
		b, _ := io.ReadAll(resp.Body)
		fmt.Printf("  [Order %d] bulk_update_status HTTP %d: %s\n", orderID, resp.StatusCode, string(b))
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