package proto

// 权限等级（§2.4）：按操作危险性分层。
// granted_level 随信封下发，执行端只做 granted >= required 数字比较。
const (
	LevelNone     = 0 // 禁用：显式关闭，不可审批绕过
	LevelRead     = 1 // 读：无副作用
	LevelWrite    = 2 // 一般写：局部、可逆、低爆炸半径
	LevelDanger   = 3 // 危险读写：破坏性/外发性/特权性/执行性/自主性
	LevelCritical = 4 // 超高危（隐藏）：只作为 required 出现，用户不可授予 ⇒ 必转人工审批
	LevelApproved = 9 // 审批通过标记：仅 procs 审批通过后的单次执行下发
)
