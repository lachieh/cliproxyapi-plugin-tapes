// Command package zips a built plugin library the way the CLIProxyAPI plugin
// store installs it: the library alone at the archive root, plus a
// sha256sum-style line for the release's checksums.txt.
//
//	go run ./scripts/package -library dist/tapes.so -archive tapes_0.1.0_linux_amd64.zip
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

func main() {
	library := flag.String("library", "", "built plugin library")
	archive := flag.String("archive", "", "zip to write; <archive>.sha256 is written next to it")
	flag.Parse()
	if *library == "" || *archive == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := writeZip(*library, *archive); err != nil {
		log.Fatal(err)
	}
	sum, err := sha256File(*archive)
	if err != nil {
		log.Fatal(err)
	}
	line := fmt.Sprintf("%s  %s\n", sum, filepath.Base(*archive))
	if err := os.WriteFile(*archive+".sha256", []byte(line), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Print(line)
}

func writeZip(library, archive string) error {
	src, err := os.Open(library)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(archive)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = filepath.Base(library)
	header.Method = zip.Deflate
	header.SetMode(0o755)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, src); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
