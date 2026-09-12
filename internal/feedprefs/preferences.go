// Package feedprefs defines personal catalog preferences shared by the server and Branch UI.
package feedprefs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Preferences struct {
	IncludeBranches        []string `json:"include_branches"`
	ExcludeBranches        []string `json:"exclude_branches"`
	ExcludeMirrored        bool     `json:"exclude_mirrored"`
	MaxSizeBytes           int64    `json:"max_size_bytes"`
	Languages              []string `json:"languages"`
	IncludeUnknownSize     bool     `json:"include_unknown_size"`
	IncludeUnknownLanguage bool     `json:"include_unknown_language"`
}

func Defaults() Preferences {
	return Preferences{IncludeBranches: []string{}, ExcludeBranches: []string{}, Languages: []string{}, IncludeUnknownSize: true, IncludeUnknownLanguage: true}
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var lang = regexp.MustCompile(`^[a-z]{2,3}$`)
var LanguageAliases = map[string]string{"eng": "en", "english": "en", "fra": "fr", "fre": "fr", "french": "fr", "deu": "de", "ger": "de", "german": "de", "spa": "es", "spanish": "es", "ita": "it", "italian": "it", "por": "pt", "portuguese": "pt", "jpn": "ja", "japanese": "ja", "zho": "zh", "chi": "zh", "chinese": "zh", "kor": "ko", "korean": "ko", "rus": "ru", "russian": "ru", "nld": "nl", "dut": "nl", "dutch": "nl", "ind": "id", "indonesian": "id", "vie": "vi", "vietnamese": "vi", "ara": "ar", "arabic": "ar"}

func Language(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.SplitN(strings.ReplaceAll(s, "_", "-"), "-", 2)[0]
	if s == "und" || s == "unknown" || s == "zxx" {
		return ""
	}
	if v, ok := LanguageAliases[s]; ok {
		return v
	}
	return s
}
func (p *Preferences) Normalize() error {
	if p.MaxSizeBytes < 0 || p.MaxSizeBytes > 1<<50 {
		return fmt.Errorf("file size limit is out of range")
	}
	for _, list := range []*[]string{&p.IncludeBranches, &p.ExcludeBranches, &p.Languages} {
		if len(*list) > 256 {
			return fmt.Errorf("too many filter values")
		}
		seen := map[string]bool{}
		out := []string{}
		for _, value := range *list {
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "" {
				continue
			}
			if list == &p.Languages {
				value = Language(value)
				if !lang.MatchString(value) {
					return fmt.Errorf("use language codes such as en, fr or ja")
				}
			} else if !uuid.MatchString(value) {
				return fmt.Errorf("invalid branch selection")
			}
			if !seen[value] {
				out = append(out, value)
				seen[value] = true
			}
		}
		sort.Strings(out)
		*list = out
	}
	return nil
}
