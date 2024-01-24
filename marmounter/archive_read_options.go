package main

import (
	"strings"

	"github.com/bmatcuk/doublestar"
)

type ArchiveReadOptions struct {
	StripPrefix      string
	AdditionalPrefix string
	IncludedGlobs    []string
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
