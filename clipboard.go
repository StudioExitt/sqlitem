package main

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// copyToClipboard copies text to the system clipboard. On a local desktop
// it uses the platform tool (pbcopy, wl-copy, xclip, xsel); in all cases it
// also emits an OSC 52 escape so it works over SSH in terminals that support
// it. Returns a short description of the method used.
func copyToClipboard(text string) (string, error) {
	var methods []string
	if err := copyWithTool(text); err == nil {
		methods = append(methods, "system")
	}
	if err := copyOSC52(text); err == nil {
		methods = append(methods, "OSC52")
	}
	if len(methods) == 0 {
		return "", errors.New("no clipboard available")
	}
	return strings.Join(methods, "+"), nil
}

func copyWithTool(text string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		// pbcopy only reaches the local pasteboard when not on SSH
		if os.Getenv("SSH_CONNECTION") == "" {
			candidates = append(candidates, []string{"pbcopy"})
		}
	case "windows":
		candidates = append(candidates, []string{"clip"})
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-copy"})
		}
		if os.Getenv("DISPLAY") != "" {
			candidates = append(candidates, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
		}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return errors.New("no clipboard tool")
}

// copyOSC52 writes the OSC 52 "set clipboard" sequence to the terminal.
// Inside tmux/screen the sequence is wrapped in a passthrough envelope.
func copyOSC52(text string) error {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	switch {
	case os.Getenv("TMUX") != "":
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	case strings.HasPrefix(os.Getenv("TERM"), "screen"):
		seq = "\x1bP" + seq + "\x1b\\"
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		_, err = os.Stdout.WriteString(seq)
		return err
	}
	defer tty.Close()
	_, err = tty.WriteString(seq)
	return err
}
