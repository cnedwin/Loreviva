// wepkg-example writes one static synthetic example. It never connects to an
// installation or treats writing a package as approval/import/publication.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	example "loreviva.local/loreviva/examples/skills/source-aware-reply"
)

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wepkg-example OUTPUT.wepkg")
	}
	raw, m, err := example.Build()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(args[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("output cannot be created exclusively")
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"package_id": m.PackageID, "version": m.PackageVersion, "content_sha256": m.ContentSHA256, "archive_sha256": hex.EncodeToString(hash[:]), "size_bytes": len(raw), "execution_ready": false})
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
