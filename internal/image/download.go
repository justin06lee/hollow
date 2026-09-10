package image

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// fetchSHA512 reads a "<hash>  <file>" checksum file and returns the hash.
func fetchSHA512(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 || len(fields[0]) != 128 {
		return "", fmt.Errorf("%s does not look like a sha512 file", url)
	}
	return strings.ToLower(fields[0]), nil
}

// download fetches url to path, verifying its SHA-512 on the way, and calls
// progress now and then with a line worth showing.
func download(ctx context.Context, url, path, wantSHA string, progress func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	part := path + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha512.New()
	var done int64
	total := resp.ContentLength
	last := time.Now()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if time.Since(last) > time.Second {
				last = time.Now()
				if total > 0 {
					progress(fmt.Sprintf("downloading %d%% (%d of %d MB)", done*100/total, done>>20, total>>20))
				} else {
					progress(fmt.Sprintf("downloading (%d MB)", done>>20))
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		os.Remove(part)
		return errors.New("checksum mismatch: the download is corrupt or the mirror is wrong")
	}
	return os.Rename(part, path)
}

// fileSHA512 hashes a file on disk.
func fileSHA512(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
