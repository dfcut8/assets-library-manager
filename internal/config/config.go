// Package config loads, creates, and validates the executable-relative JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// FileName is the fixed executable-relative configuration filename.
	FileName       = "config.json"
	maxConfigBytes = 1 << 20
)

// Config contains all startup configuration. Values are loaded once and require a restart to change.
type Config struct {
	Server     ServerConfig     `json:"server"`
	Storage    StorageConfig    `json:"storage"`
	Processing ProcessingConfig `json:"processing"`
	OpenAI     OpenAIConfig     `json:"openai"`
}

// ServerConfig controls the loopback HTTP server and graceful shutdown.
type ServerConfig struct {
	Host                   string `json:"host"`
	Port                   int    `json:"port"`
	OpenBrowser            bool   `json:"open_browser"`
	ShutdownTimeoutSeconds int    `json:"shutdown_timeout_seconds"`
}

// StorageConfig names application-root-relative runtime paths.
type StorageConfig struct {
	Database           string `json:"database"`
	IncomingDirectory  string `json:"incoming_directory"`
	ProcessedDirectory string `json:"processed_directory"`
}

// ProcessingConfig bounds local and external processing work.
type ProcessingConfig struct {
	Workers               int           `json:"workers"`
	ThumbnailMaxDimension int           `json:"thumbnail_max_dimension"`
	AnalysisMaxDimension  int           `json:"analysis_max_dimension"`
	MaxAnalysisBytes      int64         `json:"max_analysis_bytes"`
	MaxSourceBytes        int64         `json:"max_source_bytes"`
	MaxImagePixels        int64         `json:"max_image_pixels"`
	Archive               ArchiveConfig `json:"archive"`
}

// ArchiveConfig bounds ZIP archive enumeration and decompression.
type ArchiveConfig struct {
	MaxEntries                int     `json:"max_entries"`
	MaxEntryBytes             int64   `json:"max_entry_bytes"`
	MaxTotalUncompressedBytes int64   `json:"max_total_uncompressed_bytes"`
	MaxCompressionRatio       float64 `json:"max_compression_ratio"`
}

// OpenAIConfig controls future OpenAI requests. APIKey is deliberately stored only in this file.
type OpenAIConfig struct {
	APIKey              string `json:"api_key"`
	Model               string `json:"model"`
	ReasoningEffort     string `json:"reasoning_effort"`
	ImageDetail         string `json:"image_detail"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	MaxAttempts         int    `json:"max_attempts"`
	InitialRetryDelayMS int    `json:"initial_retry_delay_ms"`
}

// LogValue prevents accidental API-key disclosure when an OpenAIConfig is passed to slog.
func (c OpenAIConfig) LogValue() slog.Value {
	isConfigured := c.APIKey != ""

	return slog.GroupValue(
		slog.Bool("api_key_configured", isConfigured),
		slog.String("model", c.Model),
		slog.String("reasoning_effort", c.ReasoningEffort),
		slog.String("image_detail", c.ImageDetail),
	)
}

// Default returns the configuration written on first startup.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Host:                   "127.0.0.1",
			Port:                   7342,
			OpenBrowser:            true,
			ShutdownTimeoutSeconds: 15,
		},
		Storage: StorageConfig{
			Database:           "assets.db",
			IncomingDirectory:  "incoming",
			ProcessedDirectory: "processed",
		},
		Processing: ProcessingConfig{
			Workers:               2,
			ThumbnailMaxDimension: 320,
			AnalysisMaxDimension:  1536,
			MaxAnalysisBytes:      20 << 20,
			MaxSourceBytes:        512 << 20,
			MaxImagePixels:        100_000_000,
			Archive: ArchiveConfig{
				MaxEntries:                2000,
				MaxEntryBytes:             256 << 20,
				MaxTotalUncompressedBytes: 1 << 30,
				MaxCompressionRatio:       200,
			},
		},
		OpenAI: OpenAIConfig{
			Model:               "gpt-5.6-terra",
			ReasoningEffort:     "medium",
			ImageDetail:         "auto",
			TimeoutSeconds:      90,
			MaxAttempts:         3,
			InitialRetryDelayMS: 1000,
		},
	}
}

// LoadOrCreate writes a default config when missing, then strictly loads and validates it.
func LoadOrCreate(root string) (Config, bool, error) {
	path := filepath.Join(root, FileName)
	wasCreated := false

	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return Config{}, false, fmt.Errorf("stating config: %w", err)
		}

		created, createErr := createDefault(path, Default())
		if createErr != nil {
			return Config{}, false, fmt.Errorf("creating default config: %w", createErr)
		}
		wasCreated = created
	}

	cfg, err := load(path)
	if err != nil {
		return Config{}, wasCreated, err
	}

	return cfg, wasCreated, nil
}

func createDefault(path string, cfg Config) (created bool, returnErr error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".config.json-*")
	if err != nil {
		return false, fmt.Errorf("creating temporary file: %w", err)
	}
	tempName := temp.Name()
	tempClosed := false
	defer func() {
		if !tempClosed {
			returnErr = errors.Join(returnErr, wrapIfNotNil("closing temporary config", temp.Close()))
		}
		if err := os.Remove(tempName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("removing temporary config: %w", err))
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return false, fmt.Errorf("restricting temporary file: %w", err)
	}

	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cfg); err != nil {
		return false, fmt.Errorf("encoding defaults: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return false, fmt.Errorf("syncing defaults: %w", err)
	}
	if err := temp.Close(); err != nil {
		tempClosed = true
		return false, fmt.Errorf("closing defaults: %w", err)
	}
	tempClosed = true

	if err := os.Link(tempName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("publishing defaults: %w", err)
	}

	return true, nil
}

func load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("opening config: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		closeErr := file.Close()
		return Config{}, errors.Join(
			fmt.Errorf("reading config: %w", err),
			wrapIfNotNil("closing config", closeErr),
		)
	}
	if err := file.Close(); err != nil {
		return Config{}, fmt.Errorf("closing config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("reading config: exceeds %d bytes", maxConfigBytes)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}

	cfg := Default()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

func wrapIfNotNil(operation string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", operation, err)
}

// Validate rejects unsafe or unsupported values before runtime resources are opened.
func (c Config) Validate() error {
	if err := validateServer(c.Server); err != nil {
		return err
	}
	if err := validateStorage(c.Storage); err != nil {
		return err
	}
	if err := validateProcessing(c.Processing); err != nil {
		return err
	}
	if err := validateOpenAI(c.OpenAI); err != nil {
		return err
	}

	return nil
}

func validateServer(cfg ServerConfig) error {
	ip := net.ParseIP(cfg.Host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("server.host must be an ipv4 or ipv6 loopback address")
	}
	if cfg.Port < 1 || cfg.Port > 65_535 {
		return errors.New("server.port must be between 1 and 65535")
	}
	if cfg.ShutdownTimeoutSeconds < 1 || cfg.ShutdownTimeoutSeconds > 300 {
		return errors.New("server.shutdown_timeout_seconds must be between 1 and 300")
	}

	return nil
}

func validateStorage(cfg StorageConfig) error {
	paths := map[string]string{
		"storage.database":            cfg.Database,
		"storage.incoming_directory":  cfg.IncomingDirectory,
		"storage.processed_directory": cfg.ProcessedDirectory,
	}
	for name, path := range paths {
		if err := validateRelativePath(name, path); err != nil {
			return err
		}
	}

	if pathsOverlap(cfg.Database, cfg.IncomingDirectory) ||
		pathsOverlap(cfg.Database, cfg.ProcessedDirectory) ||
		pathsOverlap(cfg.IncomingDirectory, cfg.ProcessedDirectory) {
		return errors.New("storage paths must not overlap")
	}

	return nil
}

func validateRelativePath(name, pathValue string) error {
	if pathValue == "" || pathValue == "." {
		return fmt.Errorf("%s must be a non-empty relative path", name)
	}
	// Configuration paths use forward slashes on every supported platform. This
	// keeps validation independent of the host OS and prevents a Windows path
	// such as `..\outside` from being treated as a harmless filename on Unix.
	if strings.ContainsRune(pathValue, '\\') {
		return fmt.Errorf("%s must use forward slashes", name)
	}
	if !filepath.IsLocal(pathValue) || filepath.IsAbs(pathValue) ||
		pathpkg.IsAbs(pathValue) || hasWindowsDrivePrefix(pathValue) {
		return fmt.Errorf("%s must remain inside the application root", name)
	}
	if filepath.Clean(pathValue) != pathValue || pathpkg.Clean(pathValue) != pathValue {
		return fmt.Errorf("%s must be normalized", name)
	}

	return nil
}

func hasWindowsDrivePrefix(pathValue string) bool {
	if len(pathValue) < 2 || pathValue[1] != ':' {
		return false
	}

	first := pathValue[0]

	return first >= 'A' && first <= 'Z' || first >= 'a' && first <= 'z'
}

func pathsOverlap(left, right string) bool {
	rel, err := filepath.Rel(left, right)
	if err == nil && (rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return true
	}
	rel, err = filepath.Rel(right, left)

	return err == nil && (rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func validateProcessing(cfg ProcessingConfig) error {
	if cfg.Workers < 1 || cfg.Workers > 32 {
		return errors.New("processing.workers must be between 1 and 32")
	}
	if cfg.ThumbnailMaxDimension < 1 || cfg.ThumbnailMaxDimension > 320 {
		return errors.New("processing.thumbnail_max_dimension must be between 1 and 320")
	}
	if cfg.AnalysisMaxDimension < 1 || cfg.AnalysisMaxDimension > 4096 {
		return errors.New("processing.analysis_max_dimension must be between 1 and 4096")
	}
	if cfg.MaxAnalysisBytes < 1 || cfg.MaxSourceBytes < 1 || cfg.MaxImagePixels < 1 {
		return errors.New("processing byte and pixel limits must be positive")
	}
	if cfg.Archive.MaxEntries < 1 || cfg.Archive.MaxEntryBytes < 1 ||
		cfg.Archive.MaxTotalUncompressedBytes < 1 || cfg.Archive.MaxCompressionRatio < 1 {
		return errors.New("processing.archive limits must be positive")
	}

	return nil
}

func validateOpenAI(cfg OpenAIConfig) error {
	if cfg.APIKey != strings.TrimSpace(cfg.APIKey) {
		return errors.New("openai.api_key must not contain leading or trailing whitespace")
	}
	for _, r := range cfg.APIKey {
		if unicode.IsControl(r) {
			return errors.New("openai.api_key must not contain control characters")
		}
	}
	if cfg.Model == "" {
		return errors.New("openai.model must not be empty")
	}
	switch cfg.ImageDetail {
	case "low", "high", "auto":
	default:
		return errors.New("openai.image_detail must be low, high, or auto")
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 600 {
		return errors.New("openai.timeout_seconds must be between 1 and 600")
	}
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 10 {
		return errors.New("openai.max_attempts must be between 1 and 10")
	}
	if cfg.InitialRetryDelayMS < 1 || cfg.InitialRetryDelayMS > 60_000 {
		return errors.New("openai.initial_retry_delay_ms must be between 1 and 60000")
	}

	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(decoder); err != nil {
		return err
	}

	return requireEOF(decoder)
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		keys := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected json delimiter")
	}
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple json values are not allowed")
	}

	return err
}
