package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "gctl",
		Short: "Geeper control plane CLI",
	}
	root.AddCommand(clusterCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print gctl version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version)
		},
	}
}

func clusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage clusters",
	}
	cmd.AddCommand(clusterJoinCmd())
	return cmd
}

func clusterJoinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <token>",
		Short: "Join this machine as a worker node to a cluster",
		Args:  cobra.ExactArgs(1),
		RunE:  runJoin,
	}
}

func runJoin(_ *cobra.Command, args []string) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("must be run as root")
	}

	wrappedToken := args[0]

	// 1. Unwrap the kaas_join_ token to get API URL.
	apiURL, err := unwrapToken(wrappedToken)
	if err != nil {
		return fmt.Errorf("invalid token: %w", err)
	}
	fmt.Printf("connecting to %s\n", apiURL)

	// 2. Fetch bootstrap bundle from the API (includes k0s token + files).
	bundle, err := fetchBootstrap(apiURL, wrappedToken)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	rawToken := bundle.K0sToken

	// 3. Download k0s if needed.
	if err := ensureK0s(bundle.K0sVersion); err != nil {
		return fmt.Errorf("install k0s: %w", err)
	}

	// 4. Write bootstrap files (HAProxy cert, etc.).
	for _, f := range bundle.Files {
		if err := writeFile(f.Path, f.Content, f.Permissions); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		fmt.Printf("wrote %s\n", f.Path)
	}

	// 5. Write the raw k0s join token to a permanent path.
	const tokenPath = "/etc/k0s/join-token"
	if err := writeFile(tokenPath, rawToken, "0600"); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	// 6. Install k0s worker service.
	fmt.Println("installing k0s worker service...")
	if err := run("k0s", "install", "worker", "--token-file", tokenPath); err != nil {
		return fmt.Errorf("k0s install worker: %w", err)
	}

	// 7. Start k0s.
	fmt.Println("starting k0s...")
	if err := run("k0s", "start"); err != nil {
		return fmt.Errorf("k0s start: %w", err)
	}

	fmt.Println("worker node joined successfully")
	return nil
}

// --- k0s installation ---

func k0sReleaseVersion(version string) string {
	return strings.Replace(version, "-k0s.", "+k0s.", 1)
}

func ensureK0s(version string) error {
	const k0sBin = "/usr/local/bin/k0s"

	releaseVersion := k0sReleaseVersion(version)

	if out, err := exec.Command(k0sBin, "version").Output(); err == nil {
		installed := strings.TrimSpace(string(out))
		if installed == releaseVersion {
			fmt.Printf("k0s %s already installed\n", releaseVersion)
			return nil
		}
		fmt.Printf("k0s version mismatch (installed: %s, required: %s) — updating\n", installed, releaseVersion)
	}

	arch := k0sArch()
	url := fmt.Sprintf(
		"https://github.com/k0sproject/k0s/releases/download/%s/k0s-%s-%s",
		releaseVersion, releaseVersion, arch,
	)
	fmt.Printf("downloading k0s %s (%s) from %s\n", version, arch, url)

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	tmpPath := k0sBin + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, k0sBin); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("install binary: %w", err)
	}

	fmt.Printf("k0s %s installed at %s\n", releaseVersion, k0sBin)
	return nil
}

func k0sArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	default:
		return "amd64"
	}
}

// --- token handling ---

func unwrapToken(wrapped string) (apiURL string, err error) {
	encoded, ok := strings.CutPrefix(wrapped, "kaas_join_")
	if !ok {
		return "", fmt.Errorf("not a kaas_join_ token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	var obj map[string]string
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	apiURL = obj["a"]
	if apiURL == "" {
		return "", fmt.Errorf("missing api url in token")
	}
	return apiURL, nil
}

// --- bootstrap API ---

type bootstrapBundle struct {
	K0sVersion string          `json:"k0s_version"`
	K0sToken   string          `json:"k0s_token"`
	Files      []bootstrapFile `json:"files"`
}

type bootstrapFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Permissions string `json:"permissions"`
}

func fetchBootstrap(apiURL, wrappedToken string) (*bootstrapBundle, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL+"/api/v1/node/bootstrap", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+wrappedToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var bundle bootstrapBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &bundle, nil
}

// --- file writing ---

func writeFile(path, content, permissions string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	perm := os.FileMode(0600)
	if permissions != "" {
		n, err := strconv.ParseUint(permissions, 8, 32)
		if err == nil {
			perm = os.FileMode(n)
		}
	}
	return os.WriteFile(path, []byte(content), perm)
}

// --- subprocess ---

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
