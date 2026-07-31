// Package vcore 是 AIC 指令集 v2 的指令实现层（docs/instruction_sets_v2.md §4/§5.4）：
// fs read/write/edit + 核心 8 虚拟指令（ls/find/grep/curl/rm/mkdir/cp/mv）。
//
// 全部指令只作用于 VFS 接口，不含任何网络/认证/权限判定逻辑（curl 的网络访问
// 经 Fetcher 接口注入，SSRF 等策略由引入方配置）。权限分级表（§2.4）作为数据
// 与指令定义同包——server 事前检查与 host 纵深检查读同一张表。
//
// 跨端一致性（§2.6）由 testdata/vectors 测试向量锁定：Go（本包）与 JS（web 前端）
// 各自在 CI 跑同一套向量。
package vcore
