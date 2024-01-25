package main

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
)

type ArchiveReadOptions struct {
	StripPrefix      string
	AdditionalPrefix string
	IncludedGlobs    []string
	zipLocale        string
}

func (o *ArchiveReadOptions) SetZipLocale(locale string) error {
	if locale != "cp932" {
		return fmt.Errorf("invalid locale: %s", locale)
	}

	o.zipLocale = locale

	return nil
}

func (o *ArchiveReadOptions) ConvertZipFileName(path string) string {
	if o.zipLocale == "" {
		return path
	}

	var decoder *encoding.Decoder

	if o.zipLocale == "cp932" {
		decoder = japanese.ShiftJIS.NewDecoder()
	}

	decoded, err := decoder.String(path)

	if err != nil {
		panic(err)
	}

	return decoded
}

func (o *ArchiveReadOptions) GetFilePath(path string) string {
	matched := false

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	if len(o.IncludedGlobs) == 0 {
		matched = true
	} else {
		for _, glob := range o.IncludedGlobs {
			var err error
			matched, err = doublestar.Match(strings.ToLower(glob), strings.ToLower(path))
			if err != nil {
				matched = false
				continue
			}
			if matched {
				break
			}
		}
	}

	if o.StripPrefix != "" {
		if strings.HasPrefix(strings.ToLower(path), strings.ToLower(o.StripPrefix)) {
			path = path[len(o.StripPrefix):]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
		}
	}

	if o.AdditionalPrefix != "" {
		path = o.AdditionalPrefix + path
	}

	if !matched {
		return ""
	}

	return path
}
