package archive

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SaveGzipJSON saves data as gzipped JSON
func SaveGzipJSON(filePath string, data interface{}) error {
	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	return writeAtomic(filePath, 0600, func(file *os.File) error {
		gzWriter := gzip.NewWriter(file)

		encoder := json.NewEncoder(gzWriter)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(data); err != nil {
			gzWriter.Close()
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
		if err := gzWriter.Close(); err != nil {
			return fmt.Errorf("failed to finish gzip stream: %w", err)
		}
		return nil
	})
}

// LoadGzipJSON loads gzipped JSON data
func LoadGzipJSON(filePath string, data interface{}) error {
	// Open file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Create gzip reader
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzReader.Close()

	// Decode JSON
	decoder := json.NewDecoder(gzReader)
	if err := decoder.Decode(data); err != nil {
		return fmt.Errorf("failed to decode JSON: %w", err)
	}

	return nil
}




