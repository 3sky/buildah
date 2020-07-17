package buildah

import (
	"archive/tar"
	"fmt"
	"hash"
	"io"
	"sync"

	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type digester interface {
	io.WriteCloser
	ContentType() string
	Digest() digest.Digest
}

// A simple digester just digests its content as-is.
type simpleDigester struct {
	digester    digest.Digester
	hasher      hash.Hash
	contentType string
}

func newSimpleDigester(contentType string) digester {
	finalDigester := digest.Canonical.Digester()
	return &simpleDigester{
		digester:    finalDigester,
		hasher:      finalDigester.Hash(),
		contentType: contentType,
	}
}

func (s *simpleDigester) ContentType() string {
	return s.contentType
}

func (s *simpleDigester) Write(p []byte) (int, error) {
	return s.hasher.Write(p)
}

func (s *simpleDigester) Close() error {
	return nil
}

func (s *simpleDigester) Digest() digest.Digest {
	return s.digester.Digest()
}

// A tarFilterer passes a tarball through to an io.WriteCloser, potentially
// modifying headers as it goes.
type tarFilterer struct {
	wg         sync.WaitGroup
	pipeWriter *io.PipeWriter
	err        error
}

func (t *tarFilterer) Write(p []byte) (int, error) {
	return t.pipeWriter.Write(p)
}

func (t *tarFilterer) Close() error {
	err := t.pipeWriter.Close()
	t.wg.Wait()
	if err != nil {
		return err
	}
	return t.err
}

// newTarFilterer passes a tarball through to an io.WriteCloser, potentially
// calling filter to modify headers as it goes.
func newTarFilterer(writeCloser io.WriteCloser, filter func(hdr *tar.Header)) io.WriteCloser {
	pipeReader, pipeWriter := io.Pipe()
	tarReader := tar.NewReader(pipeReader)
	tarWriter := tar.NewWriter(writeCloser)
	filterer := &tarFilterer{
		pipeWriter: pipeWriter,
	}
	filterer.wg.Add(1)
	go func() {
		hdr, err := tarReader.Next()
		for err == nil {
			if filter != nil {
				filter(hdr)
			}
			err = tarWriter.WriteHeader(hdr)
			if err != nil {
				err = errors.Wrapf(err, "error filtering tar header for %q", hdr.Name)
				break
			}
			if hdr.Size != 0 {
				n, copyErr := io.Copy(tarWriter, tarReader)
				if copyErr != nil {
					err = errors.Wrapf(copyErr, "error filtering content for %q", hdr.Name)
					break
				}
				if n != hdr.Size {
					err = errors.Errorf("error filtering content for %q: expected %d bytes, got %d bytes", hdr.Name, hdr.Size, n)
					break
				}
			}
			hdr, err = tarReader.Next()
		}
		if err != io.EOF {
			filterer.err = err
		}
		pipeReader.Close()
		tarWriter.Close()
		writeCloser.Close()
		filterer.wg.Done()
	}()
	return filterer
}

// A tar digester digests an archive, modifying the headers it digests by
// calling a specified function to potentially modify the header that it's
// about to write.
type tarDigester struct {
	isOpen      bool
	nested      digester
	tarFilterer io.WriteCloser
}

func newTarDigester(contentType string) digester {
	nested := newSimpleDigester(contentType)
	digester := &tarDigester{
		isOpen:      true,
		nested:      nested,
		tarFilterer: nested,
	}
	return digester
}

func (t *tarDigester) ContentType() string {
	return t.nested.ContentType()
}

func (t *tarDigester) Digest() digest.Digest {
	return t.nested.Digest()
}

func (t *tarDigester) Write(p []byte) (int, error) {
	return t.tarFilterer.Write(p)
}

func (t *tarDigester) Close() error {
	if t.isOpen {
		t.isOpen = false
		return t.tarFilterer.Close()
	}
	return nil
}

// CompositeDigester can compute a digest over multiple items.
type CompositeDigester struct {
	digesters []digester
	closer    io.Closer
}

// closeOpenDigester closes an open sub-digester, if we have one.
func (c *CompositeDigester) closeOpenDigester() {
	if c.closer != nil {
		c.closer.Close()
		c.closer = nil
	}
}

// Restart clears all state, so that the composite digester can start over.
func (c *CompositeDigester) Restart() {
	c.closeOpenDigester()
	c.digesters = nil
}

// Start starts recording the digest for a new item ("", "file", or "dir").
// The caller should call Hash() immediately after to retrieve the new
// io.WriteCloser.
func (c *CompositeDigester) Start(contentType string) {
	c.closeOpenDigester()
	switch contentType {
	case "":
		c.digesters = append(c.digesters, newSimpleDigester(""))
	case "file", "dir":
		digester := newTarDigester(contentType)
		c.closer = digester
		c.digesters = append(c.digesters, digester)
	default:
		panic(fmt.Sprintf(`unrecognized content type: expected "", "file", or "dir", got %q`, contentType))
	}
}

// Hash returns the hasher for the current item.
func (c *CompositeDigester) Hash() io.WriteCloser {
	num := len(c.digesters)
	if num == 0 {
		return nil
	}
	return c.digesters[num-1]
}

// Digest returns the content type and a composite digest over everything
// that's been digested.
func (c *CompositeDigester) Digest() (string, digest.Digest) {
	c.closeOpenDigester()
	num := len(c.digesters)
	switch num {
	case 0:
		return "", ""
	case 1:
		return c.digesters[0].ContentType(), c.digesters[0].Digest()
	default:
		content := ""
		for i, digester := range c.digesters {
			if i > 0 {
				content += ","
			}
			contentType := digester.ContentType()
			if contentType != "" {
				contentType += ":"
			}
			content += contentType + digester.Digest().Encoded()
		}
		return "multi", digest.Canonical.FromString(content)
	}
}
