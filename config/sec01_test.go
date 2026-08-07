package config

import "testing"

// TestPlaintextListenBlocked 验证 SEC-01 判定：只有「监听 + 明文 + 未显式 --no-encrypt」
// 才拦；设了密钥、显式 --no-encrypt、或本端只拨出不监听，都放行。
func TestPlaintextListenBlocked(t *testing.T) {
	saveMode, saveSecret, saveNoEnc := Mode, Secret, NoEncrypt
	saveSD, saveSL := SourceDials, SinkListens
	defer func() {
		Mode, Secret, NoEncrypt = saveMode, saveSecret, saveNoEnc
		SourceDials, SinkListens = saveSD, saveSL
	}()

	set := func(mode, secret string, noEnc, srcDial, sinkListen bool) {
		m, s, n := mode, secret, noEnc
		Mode, Secret, NoEncrypt = &m, &s, &n
		SourceDials, SinkListens = srcDial, sinkListen
	}

	cases := []struct {
		name                       string
		mode, secret               string
		noEnc, srcDial, sinkListen bool
		want                       bool
	}{
		{"源监听+明文+未确认 → 拦", "reality", "", false, false, false, true},
		{"源监听+有密钥 → 放行", "reality", "k", false, false, false, false},
		{"源监听+显式no-encrypt → 放行", "reality", "", true, false, false, false},
		{"源拨出+明文 → 放行(不监听)", "reality", "", false, true, false, false},
		{"汇监听+明文+未确认 → 拦", "mirror", "", false, false, true, true},
		{"汇拨出+明文 → 放行(不监听)", "mirror", "", false, false, false, false},
		{"relay+明文+未确认 → 拦(下游恒监听)", "relay", "", false, false, false, true},
	}
	for _, c := range cases {
		set(c.mode, c.secret, c.noEnc, c.srcDial, c.sinkListen)
		if got := PlaintextListenBlocked(); got != c.want {
			t.Errorf("%s: PlaintextListenBlocked()=%v, 期望 %v", c.name, got, c.want)
		}
	}
}
