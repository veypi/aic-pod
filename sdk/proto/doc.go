// Package proto 是 AIC 指令集 v2 的协议层唯一实现（docs/instruction_sets_v2.md §6）：
// subject 构造、请求/响应信封、HMAC-SHA256 签名与 canonical 输入、caps v2 结构、
// 错误模型、路径展开算法（§2.1.1 可解析层）。
//
// server（aic）与 host（aic-pod）必须位级一致的部分全部收敛在本包，
// 双端禁止各自另写实现。
package proto
