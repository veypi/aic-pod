//
//  AicLog.swift
//  aic-pod · mobile/ios
//
//  调试日志出口：Release 构建下不输出。
//
//  为什么要有这一层：登录链路会打 server 地址、uid、cookie 名与 HttpOnly 标志。
//  凭据本身（cookie 值 / token）不进日志，但 iOS 设备日志可被任意进程经
//  sysdiagnose 读取——这些标识不该进生产日志。调试时用 Debug 构建即可看到。
//

import Foundation

enum AicLog {
    /// 调试日志：Release 构建下是空实现（消息不求值）。
    static func debug(_ message: @autoclosure () -> String) {
        #if DEBUG
        NSLog("%@", message())
        #endif
    }
}
