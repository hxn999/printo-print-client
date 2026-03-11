package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Injected at build time by -ldflags "-X main.version=vX.Y.Z"
var version = "dev"

const (
	githubRepo   = "hxn999/printo-print-client"
	installDir   = "/opt/printo"
	versionsDir  = installDir + "/versions"
	currentLink  = installDir + "/current"          // symlink → versionsDir/vX.Y.Z
	clientBin    = currentLink + "/printo-client"   // resolved via symlink
	pollInterval = 30 * time.Minute
	httpTimeout  = 60 * time.Second
)

// ── GitHub API types ──────────────────────────────────────────────────────────

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fatalf("GITHUB_TOKEN env var is required\n")
	}

	arch := archSuffix()
	fmt.Printf("printo-updater %s  arch=%s\n", version, arch)
	fmt.Printf("poll interval: %s\n\n", pollInterval)

	// Track the running client PID so we can restart it after an update.
	var clientPID int

	// Start client for the first time.
	clientPID = startClient()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		latest, err := fetchLatestRelease(token)
		if err != nil {
			logf("release check failed: %v\n", err)
			continue
		}

		if latest.TagName == version {
			logf("already up to date (%s)\n", version)
			continue
		}

		logf("new version available: %s (current: %s)\n", latest.TagName, version)

		if err := applyUpdate(latest, arch, token); err != nil {
			logf("update failed: %v — keeping current version\n", err)
			continue
		}

		// Update succeeded — restart client with new binary.
		logf("restarting client...\n")
		stopProcess(clientPID)
		clientPID = startClient()

		// Re-exec updater itself so it runs the new version next poll.
		logf("re-executing updater as %s\n", latest.TagName)
		reExecUpdater(latest.TagName)
	}
}

// ── Update pipeline ───────────────────────────────────────────────────────────

func applyUpdate(rel Release, arch, token string) error {
	tag := rel.TagName
	clientAsset := fmt.Sprintf("printo-client-%s-%s", tag, arch)
	updaterAsset := fmt.Sprintf("printo-updater-%s-%s", tag, arch)
	checksumAsset := "checksums.txt"

	// Download checksums first.
	checksums, err := downloadChecksums(rel.Assets, checksumAsset, token)
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}

	versionDir := filepath.Join(versionsDir, tag)
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", versionDir, err)
	}

	// Download and verify both binaries into versionsDir/tag/.
	for _, asset := range []string{clientAsset, updaterAsset} {
		destName := strings.TrimSuffix(strings.TrimSuffix(asset, "-"+arch), "-"+tag)
		// printo-client-vX-linux-arm64 → printo-client
		// printo-updater-vX-linux-arm64 → printo-updater
		parts := strings.SplitN(asset, "-", 3) // ["printo", "client", ...]
		destName = parts[0] + "-" + parts[1]

		destPath := filepath.Join(versionDir, destName)

		url := assetURL(rel.Assets, asset)
		if url == "" {
			// Clean up on failure.
			os.RemoveAll(versionDir)
			return fmt.Errorf("asset not found in release: %s", asset)
		}

		logf("downloading %s...\n", asset)
		if err := downloadFile(url, destPath, token); err != nil {
			os.RemoveAll(versionDir)
			return fmt.Errorf("download %s: %w", asset, err)
		}

		// Verify SHA256.
		expected, ok := checksums[asset]
		if !ok {
			os.RemoveAll(versionDir)
			return fmt.Errorf("no checksum for %s", asset)
		}
		if err := verifySHA256(destPath, expected); err != nil {
			os.RemoveAll(versionDir)
			return fmt.Errorf("checksum mismatch %s: %w", asset, err)
		}
		logf("✓ checksum OK: %s\n", asset)

		if err := os.Chmod(destPath, 0o755); err != nil {
			os.RemoveAll(versionDir)
			return err
		}
	}

	// Atomically swap the symlink: current → versionsDir/tag
	// Use a temp symlink + rename to avoid a window where current is broken.
	tmpLink := currentLink + ".tmp"
	os.Remove(tmpLink)
	if err := os.Symlink(versionDir, tmpLink); err != nil {
		os.RemoveAll(versionDir)
		return fmt.Errorf("symlink: %w", err)
	}
	if err := os.Rename(tmpLink, currentLink); err != nil {
		os.Remove(tmpLink)
		os.RemoveAll(versionDir)
		return fmt.Errorf("atomic rename symlink: %w", err)
	}

	logf("✓ updated to %s (symlink: %s → %s)\n", tag, currentLink, versionDir)
	return nil
}

// ── Client process management ─────────────────────────────────────────────────

func startClient() int {
	cmd := exec.Command(clientBin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		logf("failed to start client: %v\n", err)
		return 0
	}

	logf("client started (PID %d)\n", cmd.Process.Pid)

	// Don't Wait() here — we want non-blocking. Monitor in background so the
	// OS doesn't accumulate a zombie process.
	go func() {
		if err := cmd.Wait(); err != nil {
			logf("client exited: %v\n", err)
		}
	}()

	return cmd.Process.Pid
}

func stopProcess(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	// SIGTERM first — give client a chance to flush.
	_ = proc.Signal(syscall.SIGTERM)
	time.Sleep(3 * time.Second)
	// SIGKILL if still alive.
	_ = proc.Signal(syscall.SIGKILL)
}

// reExecUpdater replaces the current updater process with the new binary.
// This is a one-way operation — control never returns here.
func reExecUpdater(newVersion string) {
	newBin := filepath.Join(versionsDir, newVersion, "printo-updater")
	logf("exec: %s\n", newBin)
	if err := syscall.Exec(newBin, os.Args, os.Environ()); err != nil {
		logf("re-exec failed: %v — updater will stay at %s until next restart\n", err, version)
	}
}

// ── GitHub API helpers ────────────────────────────────────────────────────────

func fetchLatestRelease(token string) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadFile(url, dest, token string) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/octet-stream")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, dest)
}

func downloadChecksums(assets []Asset, name, token string) (map[string]string, error) {
	url := assetURL(assets, name)
	if url == "" {
		return nil, fmt.Errorf("checksums.txt not found in release assets")
	}

	tmp := filepath.Join(os.TempDir(), "printo-checksums.tmp")
	if err := downloadFile(url, tmp, token); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)

	data, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// sha256sum format: "<hash>  <filename>"
		parts := strings.Fields(line)
		if len(parts) == 2 {
			result[parts[1]] = parts[0]
		}
	}
	return result, nil
}

func verifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return fmt.Errorf("got %s, want %s", got, expected)
	}
	return nil
}

func assetURL(assets []Asset, name string) string {
	for _, a := range assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// ── Arch detection ────────────────────────────────────────────────────────────

func archSuffix() string {
	os_ := runtime.GOOS
	arch := runtime.GOARCH
	switch arch {
	case "arm":
		return os_ + "-armv7"
	case "arm64":
		return os_ + "-arm64"
	case "386":
		return os_ + "-386"
	default:
		return os_ + "-amd64"
	}
}

// ── Logging ───────────────────────────────────────────────────────────────────

func logf(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	fmt.Printf("[updater %s] "+format, append([]interface{}{ts}, args...)...)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format, args...)
	os.Exit(1)
}