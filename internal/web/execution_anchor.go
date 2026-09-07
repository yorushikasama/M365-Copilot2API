package web

import (
	"regexp"
	"strings"
)

var windowsWorkspacePathPattern = regexp.MustCompile(`(?i)\b[A-Z]:\\[^\r\n]+`)

// executionAnchorForPrompt returns an explicit caller-side execution contract
// only when the request already contains evidence of a Windows tool workspace.
func executionAnchorForPrompt(prompt string) string {
	low := strings.ToLower(prompt)
	hasWindows := strings.Contains(low, "windows") ||
		strings.Contains(low, "powershell") ||
		strings.Contains(low, "git bash") ||
		strings.Contains(low, "caller-side") ||
		strings.Contains(low, "caller tool")
	path := ""
	if match := windowsWorkspacePathPattern.FindString(prompt); match != "" {
		path = strings.TrimRight(match, " .,;:)]}>")
		hasWindows = true
	}
	if !hasWindows {
		return ""
	}
	pathLine := ""
	if path != "" {
		pathLine = " The request identifies the caller workspace as " + path + "."
	}
	return "EXECUTION ENVIRONMENT (caller-provided): The declared tools are real and are executed by the caller on its Windows machine, not by this upstream model. Use the declared Bash/workspace tool to inspect or modify the caller workspace and verify paths before reporting failure." + pathLine + " Do not substitute /mnt/data, a Linux container, Python sandbox, code interpreter, or cloud filesystem for the caller workspace. Do not claim the caller workspace is inaccessible merely because it is absent from the upstream model's own filesystem."
}

func appendExecutionAnchor(prompt, anchor string) string {
	if strings.TrimSpace(anchor) == "" || strings.Contains(prompt, "EXECUTION ENVIRONMENT (caller-provided):") {
		return prompt
	}
	return strings.TrimSpace(prompt) + "\n\n" + anchor
}
