package discord

import (
	"strings"
	"unicode/utf16"
)

// splitMessage breaks a large string into chunks respecting Discord's 2000-char limit.
func splitMessage(text string, limit int) []string {
	var chunks []string
	runes := []rune(text)

	for len(runes) > 0 {
		prefixLen := discordPrefixRuneCount(runes, limit)
		if prefixLen == len(runes) {
			chunks = append(chunks, string(runes))
			break
		}
		if prefixLen == 0 {
			prefixLen = 1
		}

		chunkRunes := runes[:prefixLen]
		splitIdx := -1

		for i := len(chunkRunes) - 1; i > 0; i-- {
			if chunkRunes[i] == '\n' {
				splitIdx = i
				break
			}
		}

		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i > 0; i-- {
				if (chunkRunes[i-1] == '.' || chunkRunes[i-1] == '!' || chunkRunes[i-1] == '?') && chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i > 0; i-- {
				if chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		if splitIdx == -1 {
			splitIdx = prefixLen
		}

		chunks = append(chunks, strings.TrimSpace(string(runes[:splitIdx])))
		runes = runes[splitIdx:]
		runes = []rune(strings.TrimLeft(string(runes), " \n\r"))
	}

	return chunks
}

func discordContentLength(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func discordPrefixRuneCount(runes []rune, limit int) int {
	units := 0
	for i, r := range runes {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			return i
		}
		units += width
	}
	return len(runes)
}
