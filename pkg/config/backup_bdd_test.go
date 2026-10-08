package config_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("BackupConfig", func() {
	var home string

	BeforeEach(func() {
		home = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", home)
	})

	writeUserConfig := func(yaml string) {
		dir := filepath.Join(home, "crosscodex")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600)).To(Succeed())
	}
	load := func() (*config.Config, error) {
		return config.NewLoader().Load(context.Background())
	}

	It("defaults to disabled with 26h staleness thresholds", func() {
		cfg, err := load()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Backup.DSN).To(BeEmpty())
		Expect(cfg.Backup.Destination.Backend).To(BeEmpty())
		Expect(cfg.Backup.Destination.Local.Path).To(BeEmpty())
		Expect(cfg.Backup.Destination.S3).To(Equal(config.BackupS3Config{}))
		Expect(cfg.Backup.MaxAge).To(Equal(config.BackupMaxAgeConfig{
			Postgres: 26 * time.Hour, Objects: 26 * time.Hour, NATS: 26 * time.Hour,
		}))
	})

	It("loads a local destination and max_age overrides", func() {
		writeUserConfig(`
backup:
  dsn: "postgres://backup_user@db:5432/crosscodex"
  destination:
    backend: local
    local:
      path: /srv/crosscodex-backup
  max_age:
    postgres: 2h
    objects: 3h
    nats: 4h
`)
		cfg, err := load()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Backup.DSN).To(Equal("postgres://backup_user@db:5432/crosscodex"))
		Expect(cfg.Backup.Destination.Backend).To(Equal("local"))
		Expect(cfg.Backup.Destination.Local.Path).To(Equal("/srv/crosscodex-backup"))
		Expect(cfg.Backup.MaxAge).To(Equal(config.BackupMaxAgeConfig{
			Postgres: 2 * time.Hour, Objects: 3 * time.Hour, NATS: 4 * time.Hour,
		}))
	})

	It("loads an s3 destination", func() {
		writeUserConfig(`
backup:
  dsn: "postgres://backup_user@db:5432/crosscodex"
  destination:
    backend: s3
    s3:
      bucket: crosscodex-backup
      region: eu-west-1
      endpoint: https://s3.example.test
      storage_class: STANDARD_IA
`)
		cfg, err := load()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Backup.Destination.S3).To(Equal(config.BackupS3Config{
			Bucket: "crosscodex-backup", Region: "eu-west-1",
			Endpoint: "https://s3.example.test", StorageClass: "STANDARD_IA",
		}))
	})

	It("takes backup.dsn from CROSSCODEX_BACKUP_DSN", func() {
		writeUserConfig("backup:\n  destination:\n    backend: local\n    local:\n      path: /srv/b\n")
		GinkgoT().Setenv("CROSSCODEX_BACKUP_DSN", "postgres://backup_user@db:5432/crosscodex")
		cfg, err := load()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Backup.DSN).To(Equal("postgres://backup_user@db:5432/crosscodex"))
	})

	DescribeTable("rejects invalid settings, naming the field",
		func(yaml, field string) {
			writeUserConfig(yaml)
			_, err := load()
			Expect(err).To(Or(MatchError(config.ErrInvalidConfig), MatchError(config.ErrMissingRequired)))
			Expect(err.Error()).To(ContainSubstring(field))
		},
		Entry("unknown backend",
			"backup:\n  destination:\n    backend: azure\n", "backup.destination.backend"),
		Entry("backend set without a dsn",
			"backup:\n  destination:\n    backend: local\n    local:\n      path: /srv/b\n", "backup.dsn"),
		Entry("dsn that is not a postgres URL",
			"backup:\n  dsn: \"mysql://u@h/d\"\n", "backup.dsn"),
		Entry("relative local path",
			"backup:\n  dsn: \"postgres://u@h/d\"\n  destination:\n    backend: local\n    local:\n      path: rel/dir\n",
			"backup.destination.local.path"),
		Entry("local path inside the object store",
			"storage:\n  objects:\n    backend: local\n    base_path: /srv/objects\nbackup:\n  dsn: \"postgres://u@h/d\"\n  destination:\n    backend: local\n    local:\n      path: /srv/objects/bk\n",
			"backup.destination.local.path"),
		Entry("object store inside the local path",
			"storage:\n  objects:\n    backend: local\n    base_path: /srv/b/backup\nbackup:\n  dsn: \"postgres://u@h/d\"\n  destination:\n    backend: local\n    local:\n      path: /srv/b\n",
			"backup.destination.local.path"),
		Entry("root local path overlaps every object store path",
			"storage:\n  objects:\n    backend: local\n    base_path: /srv/objects\nbackup:\n  dsn: \"postgres://u@h/d\"\n  destination:\n    backend: local\n    local:\n      path: /\n",
			"backup.destination.local.path"),
		Entry("s3 without a bucket",
			"backup:\n  dsn: \"postgres://u@h/d\"\n  destination:\n    backend: s3\n", "backup.destination.s3.bucket"),
		Entry("zero postgres max_age", "backup:\n  max_age:\n    postgres: 0s\n", "backup.max_age.postgres"),
		Entry("negative objects max_age", "backup:\n  max_age:\n    objects: -1h\n", "backup.max_age.objects"),
		Entry("zero nats max_age", "backup:\n  max_age:\n    nats: 0s\n", "backup.max_age.nats"),
	)

	It("never echoes the DSN, which may hold a password, in a validation error", func() {
		writeUserConfig("backup:\n  dsn: \"mysql://backup_user:s3cret@db/x\"\n")
		_, err := load()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("s3cret"))
	})
})
