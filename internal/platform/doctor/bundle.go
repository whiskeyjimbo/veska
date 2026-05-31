package doctor

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/audit"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// BundleOptions controls the behaviour of CreateBundle.
type BundleOptions struct {
	// VeskaHome is the veska data directory (e.g. ~/.veska).
	VeskaHome string
	// OutputDir is where the tarball is written.
	// If empty, os.TempDir() is used.
	OutputDir string
	// OllamaURL is passed to CheckEmbedder.  Defaults to http://localhost:11434 if empty.
	OllamaURL string
	// ModelName is passed to CheckEmbedder.  Defaults to nomic-embed-text if empty.
	ModelName string
}

// BundleResult is returned by CreateBundle on success.
type BundleResult struct {
	// Path is the absolute path to the written tarball.
	Path string
	// FileCount is the number of files inside the tarball.
	FileCount int
}

// CreateBundle assembles a diagnostic tarball and writes it to opts.OutputDir.
// The tarball contains a manifest, all doctor probe outputs, and a redacted
// audit log tail — see SOLO-13 §2.2 for the full spec.
func CreateBundle(opts BundleOptions) (BundleResult, error) {
	if opts.OutputDir == "" {
		opts.OutputDir = os.TempDir()
	}
	if opts.OllamaURL == "" {
		opts.OllamaURL = "http://localhost:11434"
	}
	if opts.ModelName == "" {
		opts.ModelName = "nomic-embed-text"
	}

	timestamp := time.Now().UTC().Format("20060102T150405Z")
	tarName := fmt.Sprintf("veska-bundle-%s.tar.gz", timestamp)
	tarPath := filepath.Join(opts.OutputDir, tarName)

	f, err := os.Create(tarPath)
	if err != nil {
		return BundleResult{}, fmt.Errorf("bundle: create tarball: %w", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	fileCount := 0

	addEntry := func(name string, data []byte) error {
		return writeTarEntry(tw, &fileCount, name, data)
	}

	// 1. manifest.json
	manifest := map[string]string{
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"veska_home": opts.VeskaHome,
		"go_version": runtime.Version(),
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BundleResult{}, fmt.Errorf("bundle: marshal manifest: %w", err)
	}
	if err := addEntry("manifest.json", manifestJSON); err != nil {
		return BundleResult{}, err
	}

	// 2. doctor/storage.json
	storageReport, _ := CheckStorage(opts.VeskaHome)
	storageEnv := NewEnvelope("storage", health.StatusHealthy, storageReport)
	if err := addProbeEntry(tw, &fileCount, "doctor/storage.json", storageEnv, false); err != nil {
		return BundleResult{}, err
	}

	// 3. doctor/embedder.json
	embedderReport, _ := CheckEmbedder(opts.OllamaURL, opts.ModelName)
	embedderEnv := NewEnvelope("embedder", embedderReport.Status, embedderReport)
	if err := addProbeEntry(tw, &fileCount, "doctor/embedder.json", embedderEnv, false); err != nil {
		return BundleResult{}, err
	}

	// 4. doctor/egress.json
	egressReport, _ := CheckEgress([]string{
		filepath.Join(opts.VeskaHome, "cli.sock"),
		filepath.Join(opts.VeskaHome, "mcp.sock"),
	})
	egressStatus := health.StatusHealthy
	for _, s := range egressReport.Sockets {
		if s.Status == "missing" {
			egressStatus = health.StatusBroken
			break
		}
	}
	egressEnv := NewEnvelope("egress", egressStatus, egressReport)
	if err := addProbeEntry(tw, &fileCount, "doctor/egress.json", egressEnv, false); err != nil {
		return BundleResult{}, err
	}

	// 5. doctor/config.json (redacted)
	configReport, _ := CheckConfig(opts.VeskaHome)
	configStatus := health.StatusHealthy
	if !configReport.DBExists {
		configStatus = health.StatusDegraded
	}
	configEnv := NewEnvelope("config", configStatus, configReport)
	if err := addProbeEntry(tw, &fileCount, "doctor/config.json", configEnv, true); err != nil {
		return BundleResult{}, err
	}

	// 6. doctor/service.json
	serviceReport, _ := CheckService(opts.VeskaHome)
	serviceEnv := NewEnvelope("service", serviceReport.Status, serviceReport)
	if err := addProbeEntry(tw, &fileCount, "doctor/service.json", serviceEnv, false); err != nil {
		return BundleResult{}, err
	}

	// 7. doctor/post_promotion_queue.json
	dbPath := filepath.Join(opts.VeskaHome, "veska.db")
	ppqReport, _ := CheckPostPromotionQueue(dbPath)
	ppqEnv := NewEnvelope("post_promotion_queue", ppqReport.Status, ppqReport)
	if err := addProbeEntry(tw, &fileCount, "doctor/post_promotion_queue.json", ppqEnv, false); err != nil {
		return BundleResult{}, err
	}

	// 8. audit.tail — last 100 lines, redacted
	auditTail := readAuditTail(filepath.Join(opts.VeskaHome, "audit.jsonl"), 100)
	auditTail = audit.RedactFile(auditTail)
	if err := addEntry("audit.tail", auditTail); err != nil {
		return BundleResult{}, err
	}

	// Flush all writers before we return the path.
	if err := tw.Close(); err != nil {
		return BundleResult{}, fmt.Errorf("bundle: close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return BundleResult{}, fmt.Errorf("bundle: close gzip: %w", err)
	}

	return BundleResult{Path: tarPath, FileCount: fileCount}, nil
}

// addProbeEntry marshals env to JSON (optionally redacting) and writes it to tw.
// fileCount is incremented on success.
func addProbeEntry(tw *tar.Writer, fileCount *int, name string, env Envelope, redact bool) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("bundle: marshal %s: %w", name, err)
	}
	if redact {
		data = audit.RedactFile(data)
	}
	return writeTarEntry(tw, fileCount, name, data)
}

// writeTarEntry writes data as a single tar entry named name with standard
// metadata, incrementing *fileCount on success. It is the one place tar
// headers are built, shared by the manifest/audit entries and addProbeEntry.
func writeTarEntry(tw *tar.Writer, fileCount *int, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: time.Now().UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("bundle: write header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("bundle: write data %s: %w", name, err)
	}
	*fileCount++
	return nil
}

// readAuditTail reads the audit log at path, returning the last n lines as bytes.
// Returns empty bytes if the file is missing or unreadable.
func readAuditTail(path string, n int) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return []byte{}
	}
	lines := strings.Split(string(data), "\n")
	// Remove trailing empty line from a newline-terminated file.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
