// Command manifest writes the plugin.json of one per-platform package: the
// committed plugin.json with server.executables narrowed to that platform,
// which is what build.sh puts into each archive.
//
//	go run ./cmd/manifest -platform linux-amd64 -out dist/stage/linux-amd64/plugin.json
//
// plugin.json itself is written by hand. The command only edits that one
// member and keeps the order and layout of everything else, so it works with
// any member the contract adds.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// member is one key of a JSON object with its undecoded value.
type member struct {
	Key   string
	Value json.RawMessage
}

// object is a JSON object that keeps the order of its members.
type object []member

func decodeObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		o = append(o, member{Key: token.(string), Value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

// encode writes the object compactly, the values as they are.
func (o object) encode() []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(m.Key)
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(m.Value)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func (o object) index(key string) int {
	for i, m := range o {
		if m.Key == key {
			return i
		}
	}
	return -1
}

// layout writes a document the way plugin.json is committed: two space
// indentation and a trailing newline.
func layout(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// FilterPlatform narrows plugin.json to one "<goos>-<goarch>" executable. A
// per-platform package must declare exactly the platform it ships, while the
// catalog release keeps the full map in its manifest snapshot.
func FilterPlatform(data []byte, platform string) ([]byte, error) {
	manifest, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	at := manifest.index("server")
	if at < 0 {
		return nil, errors.New("the manifest has no server block")
	}
	server, err := decodeObject(manifest[at].Value)
	if err != nil {
		return nil, fmt.Errorf("decode server: %w", err)
	}
	exeAt := server.index("executables")
	if exeAt < 0 {
		return nil, errors.New("the manifest has no server executables")
	}
	executables, err := decodeObject(server[exeAt].Value)
	if err != nil {
		return nil, fmt.Errorf("decode server.executables: %w", err)
	}
	keep := executables.index(platform)
	if keep < 0 {
		return nil, fmt.Errorf("the manifest declares no executable for %s", platform)
	}

	server[exeAt].Value = object{executables[keep]}.encode()
	manifest[at].Value = server.encode()
	return layout(manifest.encode())
}

// repoRoot resolves the module root from this file's location.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("manifest: unable to locate the command source")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

func main() {
	platform := flag.String("platform", "", `the "<goos>-<goarch>" package to write the manifest of`)
	in := flag.String("in", "", "manifest to narrow (default: the committed plugin.json)")
	out := flag.String("out", "", "output file")
	flag.Parse()

	if err := run(*platform, *in, *out); err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}
}

func run(platform, in, out string) error {
	if platform == "" || out == "" {
		return errors.New("-platform and -out are required, plugin.json is written by hand")
	}
	if in == "" {
		root, err := repoRoot()
		if err != nil {
			return err
		}
		in = filepath.Join(root, "plugin.json")
	}
	source, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	data, err := FilterPlatform(source, platform)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", out, len(data))
	return nil
}
