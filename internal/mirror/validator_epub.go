package mirror

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"
)

// EPUB validation caps. See MIRROR.md security section 2.
const (
	maxEPUBEntries       = 10000              // refuse zip with too many entries (atom-spam DoS)
	maxEPUBEntryBytes    = 10 * 1024 * 1024   // 10 MB per entry — bounds parser memory for OPF/container
	maxCompressionRatio  = 100                // uncompressed/compressed > 100 = zip bomb signal
	containerXMLPath     = "META-INF/container.xml"
)

// validateEPUB confirms path is a well-formed EPUB that's safe to store.
// It does NOT validate that the EPUB is *valid for reading*; downstream
// readers do that. Our job is to reject anything that could exploit our
// own parser or downstream tooling at promotion time.
func validateEPUB(p string, maxBytes int64) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return &ValidationError{Reason: "zip open: " + err.Error()}
	}
	defer zr.Close()

	if len(zr.File) > maxEPUBEntries {
		return &ValidationError{Reason: fmt.Sprintf("entry count %d > %d", len(zr.File), maxEPUBEntries)}
	}

	var (
		totalUncompressed uint64
		totalCompressed   uint64
		hasContainer      bool
	)
	for _, f := range zr.File {
		// Reject entries whose names are dangerous. We never extract zip
		// entries to disk, but our own metadata reader does path math on
		// these names — null bytes, parent traversal, or absolute paths
		// can confuse string handling and downstream tools.
		if err := checkEntryName(f.Name); err != nil {
			return err
		}
		// Symlinks in zip entries get explicitly rejected. Some unzip
		// tools (and our own future Audnexus cover-pulling code if it
		// ever extracts) might create them on disk, which is a classic
		// zip-slip escalation pivot.
		if f.Mode()&0xF000 == 0xA000 { // S_IFLNK
			return &ValidationError{Reason: "symlink entry: " + f.Name}
		}
		if f.UncompressedSize64 > uint64(maxEPUBEntryBytes) {
			return &ValidationError{Reason: fmt.Sprintf("entry %s uncompressed size %d > %d", f.Name, f.UncompressedSize64, maxEPUBEntryBytes)}
		}
		totalUncompressed += f.UncompressedSize64
		totalCompressed += f.CompressedSize64
		if f.Name == containerXMLPath {
			hasContainer = true
		}
	}

	// Compression-ratio bomb check. We do this in aggregate (sum
	// uncompressed / sum compressed) because per-entry ratios can be
	// dominated by tiny entries with high constant overhead.
	if totalCompressed > 0 && totalUncompressed/totalCompressed > maxCompressionRatio {
		return &ValidationError{Reason: fmt.Sprintf("compression ratio %d:1 > %d:1", totalUncompressed/totalCompressed, maxCompressionRatio)}
	}

	if !hasContainer {
		return &ValidationError{Reason: "missing META-INF/container.xml"}
	}

	opfPath, err := parseContainer(zr)
	if err != nil {
		return err
	}
	if err := parseOPF(zr, opfPath); err != nil {
		return err
	}
	return nil
}

// checkEntryName rejects zip entry names that contain traversal,
// absolute paths, null bytes, or other shenanigans. EPUB entry names
// are required to be relative POSIX paths in the spec; anything else is
// either malformed or hostile.
func checkEntryName(name string) error {
	if name == "" {
		return &ValidationError{Reason: "empty entry name"}
	}
	if strings.ContainsRune(name, 0) {
		return &ValidationError{Reason: "null byte in entry name"}
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return &ValidationError{Reason: "absolute entry name: " + name}
	}
	// Block both `..` segments and Windows-style backslashes; some EPUBs
	// from sketchy converters use backslashes which then get re-parsed
	// inconsistently across platforms.
	if strings.Contains(name, `\`) {
		return &ValidationError{Reason: "backslash in entry name: " + name}
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == ".." {
			return &ValidationError{Reason: "parent traversal in entry name: " + name}
		}
	}
	return nil
}

// container is the structure of META-INF/container.xml. We only care
// about the first rootfile's full-path attribute — that's where the OPF
// lives.
type container struct {
	XMLName   xml.Name `xml:"container"`
	Rootfiles struct {
		Rootfile []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfile"`
	} `xml:"rootfiles"`
}

func parseContainer(zr *zip.ReadCloser) (string, error) {
	var c container
	if err := decodeStrictXML(zr, containerXMLPath, &c); err != nil {
		return "", err
	}
	if len(c.Rootfiles.Rootfile) == 0 || c.Rootfiles.Rootfile[0].FullPath == "" {
		return "", &ValidationError{Reason: "container.xml has no rootfile"}
	}
	opfPath := c.Rootfiles.Rootfile[0].FullPath
	// OPF path must be a relative POSIX path inside the zip — same
	// rules as any other entry name.
	if err := checkEntryName(opfPath); err != nil {
		return "", &ValidationError{Reason: "rootfile path: " + err.(*ValidationError).Reason}
	}
	// Normalize: container.xml paths must be relative to the zip root.
	opfPath = path.Clean(opfPath)
	if strings.HasPrefix(opfPath, "..") || strings.HasPrefix(opfPath, "/") {
		return "", &ValidationError{Reason: "rootfile path escapes zip root: " + opfPath}
	}
	return opfPath, nil
}

// opfPackage is the minimum slice of OPF we need to confirm the file
// parses. We don't extract metadata here; that's the scanner's job
// after promotion.
type opfPackage struct {
	XMLName xml.Name `xml:"package"`
}

func parseOPF(zr *zip.ReadCloser, opfPath string) error {
	var pkg opfPackage
	if err := decodeStrictXML(zr, opfPath, &pkg); err != nil {
		return err
	}
	// XMLName check: encoding/xml only enforces this if the doc has any
	// element matching the struct's outer xml tag. Verify explicitly so a
	// document with no package element gets rejected.
	if pkg.XMLName.Local != "package" {
		return &ValidationError{Reason: "OPF root is not <package>"}
	}
	return nil
}

// decodeStrictXML opens entry inside zr and decodes it into dst, with a
// strict decoder that rejects DOCTYPE declarations. DOCTYPE is the entry
// point for entity-expansion attacks ("billion laughs"); Go's encoding/xml
// already refuses to expand external entities by default, but DOCTYPE
// can still define enough nested local entities to chew memory.
func decodeStrictXML(zr *zip.ReadCloser, entryName string, dst any) error {
	rc, err := openZipEntry(zr, entryName)
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, maxEPUBEntryBytes))
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &ValidationError{Reason: fmt.Sprintf("xml parse %s: %v", entryName, err)}
		}
		// xml.Directive carries the raw <!DOCTYPE ...> bytes.
		if d, ok := tok.(xml.Directive); ok {
			if isDoctype(d) {
				return &ValidationError{Reason: "DOCTYPE rejected in " + entryName}
			}
		}
		if start, ok := tok.(xml.StartElement); ok {
			if err := dec.DecodeElement(dst, &start); err != nil {
				return &ValidationError{Reason: fmt.Sprintf("xml decode %s: %v", entryName, err)}
			}
			// Successfully decoded the root element — drain the rest.
			for {
				_, err := dec.Token()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					// Trailing junk is suspect; refuse the whole file.
					return &ValidationError{Reason: fmt.Sprintf("xml trailing %s: %v", entryName, err)}
				}
			}
		}
	}
	return nil
}

func isDoctype(d xml.Directive) bool {
	s := strings.TrimSpace(string(d))
	return strings.HasPrefix(strings.ToUpper(s), "DOCTYPE")
}

// openZipEntry finds the named file in a zip reader and opens it.
// Returns ValidationError if the entry is missing.
func openZipEntry(zr *zip.ReadCloser, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, &ValidationError{Reason: fmt.Sprintf("open %s: %v", name, err)}
			}
			return rc, nil
		}
	}
	return nil, &ValidationError{Reason: "missing zip entry: " + name}
}
