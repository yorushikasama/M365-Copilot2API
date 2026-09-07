package web

import "testing"

func TestSandboxHallucinationPlainMountDataDiscussion(t *testing.T) {
	// A user or model merely mentioning /mnt/data in a normal discussion is not
	// an environment refusal and must not trigger the eject.
	if isSandboxHallucination("the script writes logs under /mnt/data in the linux reference docs") {
		t.Fatal("plain /mnt/data mention misclassified as hallucination")
	}
}

func TestSandboxHallucinationMountDataEmpty(t *testing.T) {
	if !isSandboxHallucination("无法继续落盘修改，因为当前执行环境访问不到 D:\\NetPeek，/mnt/data 也是空的") {
		t.Fatal("missing workspace mount denial not detected")
	}
}

func TestSandboxHallucinationClassicPatterns(t *testing.T) {
	cases := []string{
		"let me run that in the sandbox for you",
		"我无法执行命令，我没有 Windows 执行通道",
		"the caller workspace is not mounted in my linux container",
		"I cannot access the Windows path from here",
	}
	for _, c := range cases {
		if !isSandboxHallucination(c) {
			t.Fatalf("expected hallucination detection for %q", c)
		}
	}
}
