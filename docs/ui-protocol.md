# browser / cua 指令

browser、cua 已迁移至 [设备工具协议](hosts-tools.md)。旧 browser/1、Electron provider、共享 argv UI runner 已删除，不提供兼容入口。

`ui/1` 保留为 cua 内部操作和结果词汇，不是 aic-pod 的标准协议。RTC/NATS 使用 `hosts_tools/1` 声明的 typed 方法；目标必须明确传 `page_id` 或 `window_id`，不再选择宿主当前窗口。

现有方法、命令示例、图片读取、鉴权、生命周期及测试方式见 [当前实现说明](hosts-tools.md)。
