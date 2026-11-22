// ClawEh
// License: MIT

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecEffective(t *testing.T) {
	defaults := ExecConfig{TimeoutSeconds: 60, MaxTimeoutSeconds: 600, MaxJobs: 8, MaxJobSeconds: 86400, MaxJobOutputBytes: 10 << 20, JobLogDays: 7}
	tests := []struct {
		name string
		in   ExecConfig
		want ExecConfig
	}{
		{"absent", ExecConfig{}, defaults},
		{
			"all set",
			ExecConfig{TimeoutSeconds: 30, MaxTimeoutSeconds: 120, MaxJobs: 2, MaxJobSeconds: 3600, MaxJobOutputBytes: 1024, JobLogDays: 1},
			ExecConfig{TimeoutSeconds: 30, MaxTimeoutSeconds: 120, MaxJobs: 2, MaxJobSeconds: 3600, MaxJobOutputBytes: 1024, JobLogDays: 1},
		},
		{
			"timeout above the maximum is the maximum",
			ExecConfig{TimeoutSeconds: 900},
			ExecConfig{TimeoutSeconds: 600, MaxTimeoutSeconds: 600, MaxJobs: 8, MaxJobSeconds: 86400, MaxJobOutputBytes: 10 << 20, JobLogDays: 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Effective()
			if got.TimeoutSeconds != tt.want.TimeoutSeconds || got.MaxTimeoutSeconds != tt.want.MaxTimeoutSeconds ||
				got.MaxJobs != tt.want.MaxJobs || got.MaxJobSeconds != tt.want.MaxJobSeconds ||
				got.MaxJobOutputBytes != tt.want.MaxJobOutputBytes || got.JobLogDays != tt.want.JobLogDays {
				t.Errorf("Effective() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExecValidate(t *testing.T) {
	if err := (ExecConfig{}).Validate(); err != nil {
		t.Errorf("zero settings: %v", err)
	}
	err := ExecConfig{TimeoutSeconds: -1, MaxJobs: -2, JobLogDays: -3}.Validate()
	if err == nil {
		t.Fatal("negative settings accepted")
	}
	for _, want := range []string{
		"The shell setting tools.exec.timeout_seconds must be 0 (the default) or more.",
		"The shell setting tools.exec.max_jobs must be 0 (the default) or more.",
		"The shell setting tools.exec.job_log_days must be 0 (the default) or more.",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestLoadConfig_ExecLimits(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"tools":{"exec":{"max_jobs":3,"max_job_output_bytes":0}}}`)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Tools.Exec.Effective(); got.MaxJobs != 3 || got.MaxJobOutputBytes != DefaultExecMaxJobOutputBytes {
		t.Errorf("Effective() = %+v, want max_jobs 3 and the default output limit", got)
	}

	for _, key := range []string{"timeout_seconds", "max_timeout_seconds", "max_jobs", "max_job_seconds", "max_job_output_bytes", "job_log_days"} {
		write(`{"tools":{"exec":{"` + key + `":-1}}}`)
		if _, err := LoadConfig(configPath); err == nil || !strings.Contains(err.Error(), "tools.exec."+key) {
			t.Errorf("LoadConfig with a negative %s = %v", key, err)
		}
	}
}

// The keys round-trip through a save, are left out of the file when unset,
// and a negative value is refused.
func TestStore_ExecLimits(t *testing.T) {
	s := newTestStore(t)
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"max_jobs"`) {
		t.Errorf("an unset limit is written: %s", data)
	}
	if err = s.Update(func(c *Config) error {
		c.Tools.Exec.MaxJobs = 4
		c.Tools.Exec.JobLogDays = 2
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err = s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.Current().Tools.Exec; got.MaxJobs != 4 || got.JobLogDays != 2 {
		t.Errorf("after save and reload = %+v", got)
	}

	err = s.Update(func(c *Config) error {
		c.Tools.Exec.MaxJobSeconds = -1
		return nil
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || err.Error() != "The shell setting tools.exec.max_job_seconds must be 0 (the default) or more." {
		t.Errorf("Update with a negative limit = %v, want a ValidationError", err)
	}
	if got := s.Current().Tools.Exec.MaxJobSeconds; got != 0 {
		t.Errorf("a refused update was applied: max_job_seconds = %d", got)
	}
}
