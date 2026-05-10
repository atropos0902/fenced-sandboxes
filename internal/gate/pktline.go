package gate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// Command is one ref update from the head of a git-receive-pack request.
type Command struct{ Old, New, Ref string }

const zeroSHA = "0000000000000000000000000000000000000000"

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ReadCommands reads up to the first flush packet and returns the updates, plus
// the exact bytes it consumed so the request can be sent on unchanged.
func ReadCommands(r io.Reader) (cmds []Command, raw []byte, err error) {
	var buf bytes.Buffer
	for {
		var l [4]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return nil, nil, fmt.Errorf("pkt-line length: %w", err)
		}
		buf.Write(l[:])
		n, err := strconv.ParseUint(string(l[:]), 16, 16)
		if err != nil {
			return nil, nil, errors.New("pkt-line length is not hex")
		}
		if n == 0 {
			return cmds, buf.Bytes(), nil
		}
		if n < 5 {
			return nil, nil, fmt.Errorf("pkt-line of length %d", n)
		}
		if buf.Len() > 64<<10 {
			return nil, nil, errors.New("too many commands")
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(r, line); err != nil {
			return nil, nil, err
		}
		buf.Write(line)
		line = bytes.TrimSuffix(line, []byte("\n"))
		if bytes.HasPrefix(line, []byte("shallow ")) {
			continue
		}
		// Capabilities follow a NUL on the first command.
		if i := bytes.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		p := bytes.SplitN(line, []byte(" "), 3)
		if len(p) != 3 || !shaRE.Match(p[0]) || !shaRE.Match(p[1]) {
			return nil, nil, errors.New("unrecognised command")
		}
		cmds = append(cmds, Command{string(p[0]), string(p[1]), string(p[2])})
	}
}

// ParseRefs reads a v0 ref advertisement.
func ParseRefs(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	flushes := 0
	for {
		var l [4]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(string(l[:]), 16, 16)
		if err != nil || n > 0 && n < 4 {
			return nil, errors.New("bad ref advertisement")
		}
		if n == 0 {
			if flushes++; flushes == 2 {
				return out, nil
			}
			continue
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(r, line); err != nil {
			return nil, err
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if bytes.HasPrefix(line, []byte("#")) {
			continue
		}
		if i := bytes.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		if p := bytes.SplitN(line, []byte(" "), 2); len(p) == 2 && shaRE.Match(p[0]) {
			out[string(p[1])] = string(p[0])
		}
	}
}
