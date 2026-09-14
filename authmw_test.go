package main

import "testing"

// 已知答案测试：向量由服务器 openssl passwd -apr1 生成，保证与 nginx/htpasswd 完全兼容。
func TestApr1KnownAnswers(t *testing.T) {
	cases := []struct{ pw, hash string }{
		{"test12345", "$apr1$abcd1234$iGfa/fvS4tTntmIOY1pGD0"},
		{"SpecialPass123", "$apr1$zz99$YFtrxzrRwb8rZNXQBj2VR/"},
		{"truncates-to-8", "$apr1$longsalt$Y3toCz9UMk3TrBFPAbFdh/"},
	}
	for _, c := range cases {
		if !apr1Verify(c.pw, c.hash) {
			t.Errorf("apr1Verify(%q, %s) = false，期望 true", c.pw, c.hash)
		}
	}
}

func TestApr1RejectsWrongPassword(t *testing.T) {
	cases := []struct{ pw, hash string }{
		{"test12345", "$apr1$abcd1234$iGfa/fvS4tTntmIOY1pGD0"},
		{"SpecialPass123", "$apr1$zz99$YFtrxzrRwb8rZNXQBj2VR/"},
	}
	for _, c := range cases {
		for _, wrong := range []string{c.pw + "x", c.pw + " ", "", "Test12345"} {
			if apr1Verify(wrong, c.hash) {
				t.Errorf("错误密码 %q 被放行", wrong)
			}
		}
	}
}

func TestApr1MalformedHash(t *testing.T) {
	for _, bad := range []string{
		"", "$apr1$", "$apr1$abcd1234", "$2y$05$abcdefghijklmnopqrstuu",
		"$apr1$abcd1234$short", "$apr1$abcd1234$aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if apr1Verify("test12345", bad) {
			t.Errorf("畸形哈希被放行: %q", bad)
		}
	}
}
