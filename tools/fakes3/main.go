// Command fakes3 is a dev-only local S3 server for end-to-end tests of the
// claude-sync binaries (v1 and v2) without any real bucket or credentials.
//
//	go run ./tools/fakes3 -addr 127.0.0.1:9900 -bucket test
//	go run ./tools/fakes3 -genkey /tmp/x/age-key.txt -pass "..."   (test key file)
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/tawanorg/claude-sync/internal/crypto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9900", "listen address (loopback only)")
	bucket := flag.String("bucket", "test", "bucket to create")
	genkey := flag.String("genkey", "", "write a passphrase-derived age key to this path and exit")
	pass := flag.String("pass", "", "passphrase for -genkey (test passphrases only)")
	flag.Parse()

	if *genkey != "" {
		if err := crypto.GenerateKeyFromPassphrase(*genkey, *pass); err != nil {
			log.Fatal(err)
		}
		return
	}
	backend := s3mem.New()
	if err := backend.CreateBucket(*bucket); err != nil {
		log.Fatal(err)
	}
	log.Printf("fake S3 on http://%s bucket %s", *addr, *bucket)
	log.Fatal(http.ListenAndServe(*addr, gofakes3.New(backend).Server()))
}
