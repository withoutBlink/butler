package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// InstanceState is the JSON file butler-mcp writes after a registration so
// a local process (the Codex relay, the OpenCode plugin, Pi, DeepSeek) can
// act as that agent. ThreadID is set only for a Codex thread binding;
// older relays ignore it and keep reading agent, token and server.
type InstanceState struct {
	Agent     string `json:"agent"`
	Token     string `json:"token"`
	Server    string `json:"server"`
	Kind      string `json:"kind,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	PID       int    `json:"pid,omitempty"`
	PPID      int    `json:"ppid,omitempty"`
	WrittenAt int64  `json:"written_at,omitempty"`
}

// WriteInstanceState atomically replaces path (mode 0600) with st.
func WriteInstanceState(path string, st InstanceState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".instance-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ReadInstanceState parses a state file. It does not require every field;
// callers check the ones they need.
func ReadInstanceState(path string) (InstanceState, error) {
	var st InstanceState
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", path, err)
	}
	return st, nil
}
