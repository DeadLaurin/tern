package context

import "strings"

// frontMatter is the subset of YAML frontmatter understood by skills and
// slash commands. Unknown keys are ignored.
type frontMatter struct {
	Name        string
	Description string
	Tags        []string
}

// splitFrontMatter parses an optional leading "---" ... "---" block and
// returns the parsed metadata together with the remaining markdown body.
// A missing or unterminated frontmatter block is not an error: the whole
// input is treated as the body.
func splitFrontMatter(content string) (frontMatter, string) {
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.ReplaceAll(content, "\r\n", "\n")

	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return frontMatter{}, content
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		switch strings.TrimSpace(lines[i]) {
		case "---", "...":
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return frontMatter{}, content
	}

	return parseFrontMatterLines(lines[1:end]), strings.Join(lines[end+1:], "\n")
}

func parseFrontMatterLines(lines []string) frontMatter {
	var fm frontMatter
	listKey := ""

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if listKey == "tags" && strings.HasPrefix(trimmed, "-") {
			if item := cleanScalar(strings.TrimPrefix(trimmed, "-")); item != "" {
				fm.Tags = append(fm.Tags, item)
			}
			continue
		}

		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			listKey = ""
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		listKey = ""

		switch key {
		case "name":
			fm.Name = cleanScalar(value)
		case "description":
			fm.Description = cleanScalar(value)
		case "tags":
			if value == "" {
				// YAML block list follows on indented "- item" lines.
				listKey = "tags"
				continue
			}
			fm.Tags = append(fm.Tags, parseTagList(value)...)
		}
	}

	return fm
}

// parseTagList accepts either a YAML inline list ("[a, b]") or a plain
// comma-separated string ("a, b").
func parseTagList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	tags := make([]string, 0, len(parts))
	for _, part := range parts {
		if tag := cleanScalar(part); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// cleanScalar trims whitespace and removes one layer of matching quotes.
func cleanScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}

	switch {
	case value[0] == '"' && value[len(value)-1] == '"':
		value = value[1 : len(value)-1]
		value = strings.ReplaceAll(value, `\"`, `"`)
		value = strings.ReplaceAll(value, `\\`, `\`)
	case value[0] == '\'' && value[len(value)-1] == '\'':
		value = value[1 : len(value)-1]
		value = strings.ReplaceAll(value, `''`, `'`)
	}

	return strings.TrimSpace(value)
}
