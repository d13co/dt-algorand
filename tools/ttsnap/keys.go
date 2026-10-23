// Copyright (C) 2019-2026 Algorand Foundation Ltd.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/algorand/go-algorand/crypto"
)

// Manifests and statements are signed with deterministic Falcon-1024
// (det1024), the scheme the AVM's falcon_verify checks: the same key and
// message always give the same signature, and a statement's signature can be
// checked on chain as is.

// Roles of a key in a keys file: a producer signs the manifests of the
// snapshots it publishes; an attester signs statements about the history its
// own node built, and fetch -min-attest counts them.
const (
	roleProducer = "producer"
	roleAttester = "attester"
)

// seedPrefix starts a private key file, followed by the base64 Falcon seed.
const seedPrefix = "ttsnap-falcon1024-seed "

// sigPrefix starts a signature file: "falcon1024 <key id> <base64 signature>".
const sigPrefix = "falcon1024"

// Key is a public key of a keys file.
type Key struct {
	Name string
	Role string
	PK   crypto.FalconPublicKey
}

// keyID names a public key: the first 8 bytes of its SHA-256, in hex.
func keyID(pk crypto.FalconPublicKey) string {
	h := sha256.Sum256(pk[:])
	return hex.EncodeToString(h[:8])
}

// ID is the key's key ID.
func (k Key) ID() string { return keyID(k.PK) }

// keyLine is a keys file line for a key.
func keyLine(name, role string, pk crypto.FalconPublicKey) string {
	return fmt.Sprintf("%s %s %s", name, role, base64.StdEncoding.EncodeToString(pk[:]))
}

// readKeys reads a keys file: one key per line, "<name> <role> <base64 public
// key>", blank lines and lines starting with # ignored.
func readKeys(path string) ([]Key, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var keys []Key
	names, ids := map[string]bool{}, map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8<<10), 64<<10)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want <name> <role> <base64 public key>", path, line)
		}
		k := Key{Name: fields[0], Role: fields[1]}
		if k.Role != roleProducer && k.Role != roleAttester {
			return nil, fmt.Errorf("%s:%d: role %q, want %s or %s", path, line, k.Role, roleProducer, roleAttester)
		}
		b, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil || len(b) != len(k.PK) {
			return nil, fmt.Errorf("%s:%d: not a base64 Falcon-1024 public key", path, line)
		}
		copy(k.PK[:], b)
		if names[k.Name] || ids[k.ID()] {
			return nil, fmt.Errorf("%s:%d: %s or its key is listed twice", path, line, k.Name)
		}
		names[k.Name], ids[k.ID()] = true, true
		keys = append(keys, k)
	}
	return keys, sc.Err()
}

// readSigner reads a private key file that keygen wrote.
func readSigner(path string) (*crypto.FalconSigner, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, ok := strings.CutPrefix(strings.TrimSpace(string(b)), seedPrefix)
	var seed crypto.FalconSeed
	raw, derr := base64.StdEncoding.DecodeString(s)
	if !ok || derr != nil || len(raw) != len(seed) {
		return nil, fmt.Errorf("%s is not a ttsnap key (ttsnap keygen makes one)", path)
	}
	copy(seed[:], raw)
	signer, err := crypto.GenerateFalconSigner(seed)
	if err != nil {
		return nil, err
	}
	return &signer, nil
}

// signFile is the content of a signature file: msg signed by signer.
func signFile(signer *crypto.FalconSigner, msg []byte) ([]byte, error) {
	sig, err := signer.SignBytes(msg)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "%s %s %s\n", sigPrefix, keyID(signer.PublicKey), base64.StdEncoding.EncodeToString(sig)), nil
}

// parseSigFile returns the key ID and signature of a signature file.
func parseSigFile(b []byte) (string, crypto.FalconSignature, error) {
	fields := strings.Fields(string(b))
	if len(fields) != 3 || fields[0] != sigPrefix {
		return "", nil, errors.New("not a falcon1024 signature file")
	}
	sig, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil || len(sig) == 0 || len(sig) > crypto.FalconMaxSignatureSize {
		return "", nil, errors.New("bad signature encoding")
	}
	return fields[1], sig, nil
}

// verifySigFile checks that the signature file b signs msg by one of the keys
// of role, and returns that key.
func verifySigFile(b, msg []byte, keys []Key, role string) (Key, error) {
	id, sig, err := parseSigFile(b)
	if err != nil {
		return Key{}, err
	}
	for _, k := range keys {
		if k.Role != role || k.ID() != id {
			continue
		}
		v := crypto.FalconVerifier{PublicKey: k.PK}
		if err := v.VerifyBytes(msg, sig); err != nil {
			return Key{}, fmt.Errorf("the signature by %s (%s) does not verify", k.Name, id)
		}
		return k, nil
	}
	return Key{}, fmt.Errorf("signed by key %s, which is not a %s key of the keys file", id, role)
}

// manifestSigPrefix starts the message a manifest signature signs, followed
// by the manifest's SHA-256: never a statement (see statementPrefix).
const manifestSigPrefix = "dtalgorand-ttsnap/manifest/1\n"

func manifestMessage(manifest []byte) []byte {
	h := sha256.Sum256(manifest)
	return append([]byte(manifestSigPrefix), h[:]...)
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("o", "", "private key file to write (mode 0600); it must not exist")
	name := fs.String("name", "", "name of the key in the keys file line printed")
	role := fs.String("role", roleProducer, "role in the keys file line printed: producer or attester")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap keygen -o <key file> -name <name> [-role producer|attester]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *out == "" || *name == "" || strings.ContainsAny(*name, " \t#") || fs.NArg() != 0 || (*role != roleProducer && *role != roleAttester) {
		fs.Usage()
		os.Exit(2)
	}
	var seed crypto.FalconSeed
	if _, err := rand.Read(seed[:]); err != nil {
		return err
	}
	signer, err := crypto.GenerateFalconSigner(seed)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(seedPrefix + base64.StdEncoding.EncodeToString(seed[:]) + "\n"); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// check that the file reads back as the same key
	again, err := readSigner(*out)
	if err != nil {
		return err
	}
	if !bytes.Equal(again.PublicKey[:], signer.PublicKey[:]) {
		return errors.New("the key file does not read back as the key")
	}
	fmt.Fprintf(os.Stderr, "wrote %s (key %s); add this line to the consumers' keys file:\n", *out, keyID(signer.PublicKey))
	fmt.Println(keyLine(*name, *role, signer.PublicKey))
	return nil
}
