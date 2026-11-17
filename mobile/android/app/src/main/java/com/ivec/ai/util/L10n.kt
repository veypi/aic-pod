package com.ivec.ai.util

import android.content.Context
import org.json.JSONObject

/**
 * 壳侧文案。唯一来源是 client-bridge/l10n.json（构建时复制进 assets）。
 * 与 iOS 壳、Desktop 同一份文案，不做第二份维护。
 */
object L10n {
    private var table: Map<String, Map<String, String>> = emptyMap()

    fun load(context: Context) {
        table = try {
            val raw = context.assets.open("l10n.json").bufferedReader().use { it.readText() }
            val root = JSONObject(raw)
            val languages = root.keys().asSequence().associateWith { code ->
                val values = root.getJSONObject(code)
                values.keys().asSequence().associateWith { key -> values.getString(key) }
            }
            languages
        } catch (_: Exception) {
            emptyMap()
        }
    }

    /** 取文案；缺 key 时回落到中文，再缺就返回 key 本身。 */
    fun text(key: String, language: String = "zh-CN"): String =
        table[language]?.get(key)
            ?: table["zh-CN"]?.get(key)
            ?: key
}
