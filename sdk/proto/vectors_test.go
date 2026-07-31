package proto

// 固定向量值（sign_test.go）。由实现生成一次后硬编码锁定：
// 任何派生参数、canonical 结构、编码方式的变化都会使测试失败——
// 这正是双端零漂移的保障，修改即协议变更。

const (
	vecKConnect = "N3kygIFMBZFwCrfzkwcZFbgqt5uf7Rzz6-_oevYQoSA"
	vecKServer  = "tXA-GUsQs-n0Bmmt09z7140onDlU1PCZq6qmkbpJoUE"
	vecKTool    = "VMTmXHIBYWUnxQenEBwsqJKgDG_zZdAQXMRyksiwIIg"

	vecToolReqSig = "k_47wuS-A22foxdJ4pu0j5VNdP3cS-Sp_t11_lYVNRg"

	vecConnectToken = "e1.host_vec01.eyJhZ2VudF92ZXJzaW9uIjoidjAuMy4wIiwiZGV2aWNlX25hbWUiOiJuYXMtMDEiLCJkZXZpY2VfdHlwZSI6ImNsaSJ9.1767225600000.BBBBBBBBBBBBBBBBBBBBBB.1jfc9uC9K-l1-CCI48xkX0yaOgLtkhmcbxPBeDmL1JI"
)
