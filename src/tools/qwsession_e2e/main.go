// qwsession_e2e：用**真实适配器**对**真上游**跑多轮会话，验证 0.10.0 会话复用。
//
// 场景还原用户投诉：历史很大（>32KB）+ 新问题很短。
//
//	T1 第一轮短问题（新建会话，发全量）
//	T2 客户端回填助手正文 + 新问题（应复用会话，只发最新一轮）
//	T3 历史里塞 40KB 旧内容 + 换掉 system 提示 + 新问题（仍应复用，只发最新一轮）
//	T4 负对照：**没有台账**（新适配器实例）的 40KB 历史 → 应如实报 PromptTooLong
//
// 用法：go run ./tools/qwsession_e2e <creds.json>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"poolgate/internal/adapter/qwenwork"
	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const uid = "9567c2c8-3ad2-4443-bee7-95abcef69691"

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var cred struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		panic(err)
	}
	c := channel.Credential{UID: uid, AccessToken: cred.Auth.AccessToken}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	a := qwenwork.New()

	t1 := []channel.Message{{Role: "user", Content: "请记住这个数字：四十二。只回复：好的"}}
	r1, err1 := turn(ctx, a, c, t1)
	fmt.Printf("T1（新建会话·全量）答复=%q err=%v\n", r1, err1)

	t2 := append(append([]channel.Message{}, t1...),
		channel.Message{Role: "assistant", Content: r1},
		channel.Message{Role: "user", Content: "我让你记住的数字是多少？只回复数字。"})
	r2, err := turn(ctx, a, c, t2)
	fmt.Printf("T2（应复用会话）答复=%q err=%v\n", r2, err)

	// T3：历史里塞 40KB 旧内容（模拟长对话），并换掉 system 提示。
	filler := strings.Repeat("这是一段很长的历史内容，不属于任何真实对话。", 40*1024/len("这是一段很长的历史内容，不属于任何真实对话。")+1)
	t3 := append([]channel.Message{
		{Role: "system", Content: "你是测试助手。" + filler},
	}, t2...)
	t3 = append(t3,
		channel.Message{Role: "assistant", Content: r2},
		channel.Message{Role: "user", Content: "最后确认一次：那个数字是多少？只回复数字。"})
	fmt.Printf("T3 请求历史体积=%d 字节（远超上游 32768 字节单帧上限）\n", sizeOf(t3))
	r3, err := turn(ctx, a, c, t3)
	fmt.Printf("T3（历史 40KB+，应靠复用只发最新一轮）答复=%q err=%v\n", r3, err)

	// T4：负对照 —— 新适配器（无台账），同样体积的历史必须**如实报超限**。
	a2 := qwenwork.New()
	_, err4 := turn(ctx, a2, c, t3)
	k, _ := errs.KindOf(err4)
	fmt.Printf("T4（无台账·负对照）errKind=%v err=%v\n", k, err4)

	fmt.Println()
	fmt.Println("=== 判定 ===")
	fmt.Printf("T1 正常：%v\n", err1 == nil)
	fmt.Printf("T2 上下文保住（答 42）：%v\n", strings.Contains(r2, "42") || strings.Contains(r2, "四十二"))
	fmt.Printf("T3 长历史下仍可用（答 42）：%v\n", strings.Contains(r3, "42") || strings.Contains(r3, "四十二"))
	fmt.Printf("T4 无台账时如实报 PromptTooLong：%v\n", k == errs.PromptTooLong)
}

func turn(ctx context.Context, a *qwenwork.Adapter, c channel.Credential, msgs []channel.Message) (string, error) {
	st, err := a.Chat(ctx, &c, channel.ChatRequest{Model: "flash", Messages: msgs})
	if err != nil {
		return "", err
	}
	defer st.Close()
	var b strings.Builder
	for {
		chunk, err := st.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return b.String(), nil
			}
			return b.String(), err
		}
		for _, ch := range chunk.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
}

func sizeOf(msgs []channel.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}
