// Command releasetool creates the signing key for releases and signs their
// SHA256SUMS file. It is used by the release workflow, not shipped to servers.
//
//	releasetool keygen -out release.key      writes the private key (0600), prints the public key
//	releasetool pubkey -key release.key      prints the public key
//	releasetool fingerprint -key release.key prints the SHA-256 fingerprint of the public key
//	releasetool sign -key release.key FILE   prints the base64 signature of FILE
//	releasetool sign -key-env VAR FILE       same, with the private key taken from $VAR
//	releasetool verify -pub KEY FILE SIGFILE exits non-zero unless SIGFILE signs FILE for the public key
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"

	"appdeploy/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: releasetool keygen|pubkey|sign ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "fingerprint":
		err = fingerprint(os.Args[2:])
	case "pubkey":
		err = pubkey(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasetool:", err)
		os.Exit(1)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file to write the private key to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s exists; refusing to overwrite a signing key", *out)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	fmt.Fprintf(os.Stderr, "private key written to %s: keep it secret and backed up; the line above is the public key to build into releases\n", *out)
	return nil
}

// loadKey reads a base64 ed25519 seed from a file or an environment variable.
func loadKey(file, env string) (ed25519.PrivateKey, error) {
	var raw string
	switch {
	case file != "" && env != "":
		return nil, fmt.Errorf("give -key or -key-env, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	case env != "":
		raw = os.Getenv(env)
		if raw == "" {
			return nil, fmt.Errorf("$%s is empty", env)
		}
	default:
		return nil, fmt.Errorf("-key or -key-env is required")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("the private key is not a base64 ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func pubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	file := fs.String("key", "", "private key file")
	env := fs.String("key-env", "", "environment variable holding the private key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := loadKey(*file, *env)
	if err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey)))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	file := fs.String("key", "", "private key file")
	env := fs.String("key-env", "", "environment variable holding the private key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: sign (-key FILE | -key-env VAR) FILE")
	}
	k, err := loadKey(*file, *env)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(ed25519.Sign(k, data)))
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pub := fs.String("pub", "", "base64 public key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *pub == "" {
		return fmt.Errorf("usage: verify -pub KEY FILE SIGFILE")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*pub))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("the public key is not a base64 ed25519 key")
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	if err := update.VerifySums(ed25519.PublicKey(key), data, sig); err != nil {
		return err
	}
	fmt.Println("signature is valid")
	return nil
}

func fingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	file := fs.String("key", "", "private key file")
	env := fs.String("key-env", "", "environment variable holding the private key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := loadKey(*file, *env)
	if err != nil {
		return err
	}
	fmt.Println(update.Fingerprint(k.Public().(ed25519.PublicKey)))
	return nil
}
