package config

import "fmt"

var (
	validTLSModes        = map[string]bool{"off": true, "server-only": true, "mutual": true}
	validStorageBackends = map[string]bool{"local": true, "s3": true}
	validLogLevels       = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats      = map[string]bool{"text": true, "json": true}
)

func validate(cfg *Config, tracker *sourceTracker) error {
	if err := validateTLS(&cfg.TLS, tracker); err != nil {
		return err
	}
	if err := validateStorage(&cfg.Storage, tracker); err != nil {
		return err
	}
	if err := validateLogging(&cfg.Logging, tracker); err != nil {
		return err
	}
	return nil
}

func validateTLS(tls *TLSConfig, tracker *sourceTracker) error {
	if tls.Mode != "" && !validTLSModes[tls.Mode] {
		return fmt.Errorf("tls.mode %q%s must be one of off, server-only, mutual: %w",
			tls.Mode, formatSource(tracker, "tls.mode"), ErrInvalidConfig)
	}

	switch tls.Mode {
	case "mutual":
		if tls.Cert == "" || tls.Key == "" || tls.CA == "" {
			return fmt.Errorf("tls.mode mutual%s requires cert, key, and ca: %w",
				formatSource(tracker, "tls.mode"), ErrInvalidConfig)
		}
	case "server-only":
		if tls.Cert == "" || tls.Key == "" {
			return fmt.Errorf("tls.mode server-only%s requires cert and key: %w",
				formatSource(tracker, "tls.mode"), ErrInvalidConfig)
		}
	}

	// Validate per-target overrides
	for name, override := range tls.Targets {
		if override.Mode != "" && !validTLSModes[override.Mode] {
			return fmt.Errorf("tls.targets.%s.mode %q must be one of off, server-only, mutual: %w",
				name, override.Mode, ErrInvalidConfig)
		}
	}

	return nil
}

func validateStorage(s *StorageConfig, tracker *sourceTracker) error {
	if s.Objects.Backend != "" && !validStorageBackends[s.Objects.Backend] {
		return fmt.Errorf("storage.objects.backend %q%s must be one of local, s3: %w",
			s.Objects.Backend, formatSource(tracker, "storage.objects.backend"), ErrInvalidConfig)
	}
	return nil
}

func validateLogging(l *LoggingConfig, tracker *sourceTracker) error {
	if l.Level != "" && !validLogLevels[l.Level] {
		return fmt.Errorf("logging.level %q%s must be one of debug, info, warn, error: %w",
			l.Level, formatSource(tracker, "logging.level"), ErrInvalidConfig)
	}
	if l.Format != "" && !validLogFormats[l.Format] {
		return fmt.Errorf("logging.format %q%s must be one of text, json: %w",
			l.Format, formatSource(tracker, "logging.format"), ErrInvalidConfig)
	}
	return nil
}
