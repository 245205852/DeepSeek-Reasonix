package responses

import "testing"

func TestSharedWindowOutputBudgetCapability(t *testing.T) {
	deepseek := &client{vendor: "deepseek", maxOutputTokens: 128 * 1024}
	if !deepseek.SharesContextWindow() || deepseek.OutputBudget() != 128*1024 {
		t.Fatalf("DeepSeek capability = shared:%v budget:%d", deepseek.SharesContextWindow(), deepseek.OutputBudget())
	}
	mimo := &client{vendor: "mimo", maxOutputTokens: 128000}
	if mimo.SharesContextWindow() {
		t.Fatal("MiMo mode must stay unchanged until its shared-window contract is verified")
	}
}
