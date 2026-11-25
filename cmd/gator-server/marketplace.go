package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onegator/gator/internal/server/marketplace"
)

const marketplaceUsage = `usage: gator-server marketplace <command>
  checksum <package dir>          print a package's checksum, as index.json carries it
  index <registry dir> [--check]  write index.json from the packages in the registry (--check: only say whether it is current)
  keygen <private key file>       make a signing key: the private key goes to the file (0600), the public key is printed
  sign <registry dir> <key file>  sign index.json into index.json.sig
  verify <registry dir> [key]     read the registry as the server would; exit 1 if anything is turned away`

// indexLifetime is how long a published index is good for.
const indexLifetime = 180 * 24 * time.Hour

// marketplaceCmd works with registries on disk: what a registry's maintainer and its CI run.
func marketplaceCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(marketplaceUsage)
	}
	kinds, lim := marketplace.DefaultKinds(), marketplace.DefaultLimits
	switch {
	case args[0] == "checksum" && len(args) == 2:
		files, err := marketplace.ReadFiles(os.DirFS(args[1]), ".", lim)
		if err != nil {
			return err
		}
		fmt.Println(files.Checksum())
		return nil
	case args[0] == "index" && (len(args) == 2 || (len(args) == 3 && args[2] == "--check")):
		idx, err := marketplace.Scan(os.DirFS(args[1]), kinds, lim)
		if err != nil {
			return err
		}
		path := filepath.Join(args[1], marketplace.IndexFile)
		var prev marketplace.Index
		if raw, err := os.ReadFile(path); err == nil {
			if prev, err = marketplace.ParseIndex(raw, kinds.Names()); err != nil {
				return err
			}
		}
		if len(args) == 3 {
			if !prev.SameItems(idx) {
				return fmt.Errorf("%s is not current: run gator-server marketplace index %s", path, args[1])
			}
			fmt.Printf("%s is current (%d packages, serial %d)\n", path, len(idx.Items), prev.Serial)
			return nil
		}
		// A change publishes a new serial, valid for half a year: the registry is signed again
		// at least that often, and a server never takes an older index than one it has seen.
		idx.Serial, idx.Expires = prev.Serial, prev.Expires
		if !prev.SameItems(idx) || prev.Serial == 0 {
			idx.Serial = prev.Serial + 1
			idx.Expires = time.Now().UTC().Add(indexLifetime).Truncate(time.Second).Format(time.RFC3339)
		}
		fmt.Printf("%d packages, serial %d, expires %s\n", len(idx.Items), idx.Serial, idx.Expires)
		return os.WriteFile(path, marketplace.MarshalIndex(idx), 0o644)
	case args[0] == "keygen" && len(args) == 2:
		pub, priv, err := marketplace.GenerateKey()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(priv + "\n"); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Println(pub)
		return nil
	case args[0] == "sign" && len(args) == 3:
		key, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		index, err := os.ReadFile(filepath.Join(args[1], marketplace.IndexFile))
		if err != nil {
			return err
		}
		sig, err := marketplace.Sign(string(key), index)
		if err != nil {
			return err
		}
		pub, _ := marketplace.PublicKeyOf(string(key))
		fmt.Println("signed with", pub)
		return os.WriteFile(filepath.Join(args[1], marketplace.SignatureFile), sig, 0o644)
	case args[0] == "verify" && (len(args) == 2 || len(args) == 3):
		key := ""
		if len(args) == 3 {
			key = args[2]
		}
		v, err := marketplace.NewVerifier(key, lim)
		if err != nil {
			return err
		}
		contents, err := marketplace.Read(os.DirFS(args[1]), v, kinds, lim)
		if err != nil {
			return err
		}
		for _, p := range contents.Packages {
			fmt.Printf("ok       %s %s\n", p.Item.Key(), p.Item.Version)
		}
		var why []string
		for _, r := range contents.Rejected {
			fmt.Printf("rejected %s %s: %s\n", r.Item.Key(), r.Item.Version, r.Reason)
			why = append(why, r.Item.Key())
		}
		if len(why) > 0 {
			return fmt.Errorf("%d packages turned away: %s", len(why), strings.Join(why, ", "))
		}
		return nil
	}
	return errors.New(marketplaceUsage)
}
