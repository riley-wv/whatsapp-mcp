package paths

import (
	"os"
	"path/filepath"
)

// DataDir is the base data directory for the application.
const DataDir = "./data"

// Data subdirectories for organizing different types of data.
const (
	DataDBDir    = DataDir + "/db"
	DataMediaDir = DataDir + "/media"
)

// Storage paths for migrations and other persistent data.
const (
	MigrationsDir = "storage/migrations"
)

// File paths for databases, logs, and other files.
const (
	MessagesDBPath     = DataDBDir + "/messages.db"
	WhatsAppAuthDBPath = DataDBDir + "/whatsapp_auth.db"
	WhatsAppLogPath    = DataDir + "/whatsapp.log"
	QRCodePath         = "./qr.png"
)

// InstancePaths contains all filesystem paths for one isolated WhatsApp setup.
type InstancePaths struct {
	ID                 string
	DataDir            string
	DBDir              string
	MediaDir           string
	MessagesDBPath     string
	WhatsAppAuthDBPath string
	WhatsAppLogPath    string
	QRCodePath         string
}

// DefaultInstancePaths returns the original single-instance paths.
func DefaultInstancePaths() InstancePaths {
	return InstancePaths{
		ID:                 "default",
		DataDir:            DataDir,
		DBDir:              DataDBDir,
		MediaDir:           DataMediaDir,
		MessagesDBPath:     MessagesDBPath,
		WhatsAppAuthDBPath: WhatsAppAuthDBPath,
		WhatsAppLogPath:    WhatsAppLogPath,
		QRCodePath:         QRCodePath,
	}
}

// TenantInstancePaths returns paths for one tenant under ./data/tenants/{id}.
func TenantInstancePaths(id string) InstancePaths {
	dataDir := filepath.Join(DataDir, "tenants", id)
	dbDir := filepath.Join(dataDir, "db")
	return InstancePaths{
		ID:                 id,
		DataDir:            dataDir,
		DBDir:              dbDir,
		MediaDir:           filepath.Join(dataDir, "media"),
		MessagesDBPath:     filepath.Join(dbDir, "messages.db"),
		WhatsAppAuthDBPath: filepath.Join(dbDir, "whatsapp_auth.db"),
		WhatsAppLogPath:    filepath.Join(dataDir, "whatsapp.log"),
		QRCodePath:         filepath.Join(dataDir, "qr.png"),
	}
}

// EnsureDataDirectories ensures that all required data directories exist.
func EnsureDataDirectories() error {
	return EnsureInstanceDirectories(DefaultInstancePaths())
}

// EnsureInstanceDirectories ensures that all directories for an instance exist.
func EnsureInstanceDirectories(instance InstancePaths) error {
	dirs := []string{
		instance.DataDir,
		instance.DBDir,
		instance.MediaDir,
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return nil
}

// GetMediaPath returns the full path for a media file given its relative path.
func GetMediaPath(relativePath string) string {
	return filepath.Join(DataMediaDir, relativePath)
}

// GetInstanceMediaPath returns the full path for an instance media file.
func GetInstanceMediaPath(instance InstancePaths, relativePath string) string {
	return filepath.Join(instance.MediaDir, relativePath)
}
