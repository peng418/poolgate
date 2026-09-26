package deepseek

// guestpow_live_test.go —— 对**真上游**的手动联调测试（默认跳过，不进 CI）。
//
// 为什么值得单独留一个文件：这个包里关于 PoW 的两个结论（40300 的真身是
// POW_HEADER_ERROR、以及 `X-DS-Guest-PoW-Response` 能被上游接受）**只有在真上游上
// 才能证实** —— 桩测试只能证明我们「按约定把头发了出去」。改动 PoW 相关代码后跑一次：
//
//	PG_LIVE=1 go test ./internal/adapter/deepseek/ -run TestLiveGuestPoW -v
//
// 安全性：只打 create_guest_challenge 和 login_by_mobile_sms（带一个明显假的验证码），
// **不会真的发短信**、不碰任何已有账号的凭证、不写盘。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// liveSnapshot 是一个固定的假身份（这条路径不需要真设备 id）。
func liveSnapshot() smsSnapshot {
	return smsSnapshot{
		mobile:   "13800138000",
		areaCode: "+86",
		code:     "000000",
		deviceID: "00000000-0000-4000-8000-000000000000",
	}
}

// liveLoginBody 是短信登录的请求体（与 loginByMobileSMS 里的一致；这里自己拼一份，
// 是为了能单独控制「带不带 PoW 头」这一件事）。
func liveLoginBody(s smsSnapshot) []byte {
	b, _ := json.Marshal(map[string]any{
		"region": "CN", "locale": "zh_CN",
		"mobile_number": s.mobile, "area_code": s.areaCode,
		"sms_verification_code": s.code, "device_id": s.deviceID, "os": "android",
	})
	return b
}

func TestLiveGuestPoW(t *testing.T) {
	if os.Getenv("PG_LIVE") == "" {
		t.Skip("需要 PG_LIVE=1（会打真上游，默认跳过）")
	}
	ctx := context.Background()
	a := New()
	s := liveSnapshot()

	// ① 对照组：手搓一个**不带 PoW 头**的请求，确认真上游的答案是 40300 Missing Header
	//    —— 这就是真机上那个「验证码登录失败：上游原话：Missing Header」的形状。
	func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			a.base+"/users/login_by_mobile_sms", strings.NewReader(string(liveLoginBody(s))))
		if err != nil {
			t.Fatalf("构造对照请求失败：%v", err)
		}
		for k, v := range clientHeaders(guestCred(s)) {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := a.clientFor(guestCred(s)).Do(req)
		if err != nil {
			t.Fatalf("对照请求发送失败：%v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		code, msg, _ := parseBiz(raw)
		t.Logf("① 不带 PoW：HTTP %d  code=%d  msg=%q", resp.StatusCode, code, msg)
		if code != 40300 {
			t.Fatalf("对照组的结论变了：想要 40300(Missing Header)，实际 code=%d msg=%q 原始=%s", code, msg, raw)
		}
	}()

	// ② 实验组：走正常路径（authCall 会自己取挑战、解出来、带上头）。
	//    顺手把「取挑战 + 解挑战」的耗时量出来 —— 这段是串在用户操作路径上的，
	//    慢了就得改（比如在等用户点题时先预取），所以必须有个数。
	tPow := time.Now()
	h, perr := a.guestPow(ctx, s, "/users/login_by_mobile_sms")
	t.Logf("② 取挑战+解 PoW 耗时 %v（err=%v，头长 %d）", time.Since(tPow).Round(time.Millisecond), perr, len(h))

	raw, status, err := a.authCall(ctx, s, "/users/login_by_mobile_sms", liveLoginBody(s))
	if err != nil {
		t.Fatalf("带 PoW 的请求失败：%v", err)
	}
	code, msg, _ := parseBiz(raw)
	t.Logf("② 带 guest PoW：HTTP %d  code=%d  msg=%q", status, code, msg)
	if code == 40300 || strings.Contains(strings.ToUpper(msg), "MISSING HEADER") {
		t.Fatalf("带了 guest PoW 还是被判缺头 —— 这个头上游不认或没带上：%s", raw)
	}
	t.Logf("✅ PoW 头被上游接受：错误已经不再是「缺头」，而是业务原因（假验证码 → 符合预期）")
}
