package app

import "context"

// optCtx 从可选 ctx 参数里取第一个非 nil 的 context。
//
// 这些网络辅助函数(uploadBytes / uploadPost / getXSRF / doGeminiRequest)散落在
// 很多调用点,其中大部分没有请求 context。把它们改成可变参数 ctx,既能让流式
// 路径传播取消(客户端断开 → 上游调用取消 → slot 立即释放),又不逼所有旧调用
// 点跟改。返回 nil 时调用方保持旧行为(不调 WithContext)。
func optCtx(ctxs []context.Context) context.Context {
	for _, c := range ctxs {
		if c != nil {
			return c
		}
	}
	return nil
}
