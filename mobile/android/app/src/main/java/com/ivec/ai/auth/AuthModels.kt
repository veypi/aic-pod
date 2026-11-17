package com.ivec.ai.auth

import android.util.Base64
import org.json.JSONObject
import java.security.MessageDigest

/**
 * 动态 Web 会话凭据模型与 JWT / Set-Cookie 解析。
 *
 * 令牌只从 HttpOnly Cookie 取，响应 body 只提供用户信息——
 * 与 iOS 壳（AuthModels.swift）、Desktop（auth-cookies.cjs）同一套口径。
 */
data class AuthUser(
    val id: String,
    val username: String? = null,
    val nickname: String? = null
) {
    val displayName: String get() = nickname ?: username ?: id

    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .apply {
            username?.let { put("username", it) }
            nickname?.let { put("nickname", it) }
        }
}

data class AuthCredentials(
    val accessToken: String,
    val refreshToken: String,
    val accessCookieName: String,
    val refreshCookieName: String,
    val user: AuthUser,
    val expiresAtMillis: Long
) {
    fun toJson(): JSONObject = JSONObject()
        .put("accessToken", accessToken)
        .put("refreshToken", refreshToken)
        .put("accessCookieName", accessCookieName)
        .put("refreshCookieName", refreshCookieName)
        .put("user", user.toJson())
        .put("expiresAt", expiresAtMillis)

    companion object {
        fun fromJson(json: JSONObject): AuthCredentials {
            val user = json.getJSONObject("user")
            return AuthCredentials(
                accessToken = json.getString("accessToken"),
                refreshToken = json.getString("refreshToken"),
                accessCookieName = json.getString("accessCookieName"),
                refreshCookieName = json.getString("refreshCookieName"),
                user = AuthUser(
                    id = user.getString("id"),
                    username = user.optString("username").takeIf { it.isNotEmpty() },
                    nickname = user.optString("nickname").takeIf { it.isNotEmpty() }
                ),
                expiresAtMillis = json.getLong("expiresAt")
            )
        }
    }
}

/** 对外公开的登录态：不含任何 token。 */
data class AuthState(
    val signedIn: Boolean,
    val user: AuthUser?,
    val expiresAtMillis: Long
) {
    fun toJson(): JSONObject = JSONObject()
        .put("signedIn", signedIn)
        .put("expiresAt", expiresAtMillis)
        .put("user", user?.toJson() ?: JSONObject.NULL)
}

/** JWT 解析：只读载荷，不验签（服务端才是验签方）。 */
object JWT {
    fun payload(token: String): JSONObject? = try {
        val parts = token.split(".")
        if (parts.size != 3) return null
        val segment = parts[1].replace('-', '+').replace('_', '/')
        val padded = segment + "=".repeat((4 - segment.length % 4) % 4)
        JSONObject(String(Base64.decode(padded, Base64.DEFAULT)))
    } catch (_: Exception) {
        null
    }

    /** 用户身份：uid 或 sub，必须是 32 位小写十六进制。 */
    fun uid(token: String): String? {
        val payload = payload(token) ?: return null
        val raw = payload.optString("uid").ifEmpty { payload.optString("sub") }
        if (raw.length != 32) return null
        return if (raw.all { it in "0123456789abcdef" }) raw else null
    }

    fun type(token: String): String? = payload(token)?.optString("type")?.ifEmpty { null }

    /** access token 的过期时刻（exp 是秒级时间戳）。 */
    fun expiresAt(token: String, fallback: Long): Long {
        val exp = payload(token)?.optDouble("exp", Double.NaN) ?: return fallback
        if (exp.isNaN()) return fallback
        return (exp * 1000).toLong()
    }
}

/** 一条 Set-Cookie。 */
data class ParsedCookie(
    val name: String,
    val value: String,
    val path: String = "/",
    val domain: String = "",
    val secure: Boolean = false,
    val httpOnly: Boolean = false,
    val expiresMillis: Long = 0
)

/** Set-Cookie 头解析。 */
object SetCookie {
    private val cookieName = Regex("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+=")

    /** 把可能合并在一起的 Set-Cookie 头拆成多条（Expires 里的逗号不会被误切）。 */
    fun split(raw: String): List<String> {
        val result = mutableListOf<String>()
        var cursor = 0
        while (cursor < raw.length) {
            val comma = raw.indexOf(',', cursor)
            if (comma < 0) break
            val rest = raw.substring(comma + 1).trimStart()
            if (cookieName.containsMatchIn(rest)) {
                result += raw.substring(cursor, comma).trim()
                cursor = comma + 1
            } else {
                cursor = comma + 1
            }
        }
        result += raw.substring(cursor).trim()
        return result.filter { it.isNotEmpty() }
    }

    /** 解析一条 Set-Cookie。 */
    fun parse(line: String): ParsedCookie? {
        val parts = line.split(";").map { it.trim() }
        val head = parts.firstOrNull() ?: return null
        val index = head.indexOf('=')
        if (index <= 0) return null
        var cookie = ParsedCookie(name = head.substring(0, index).trim(), value = head.substring(index + 1).trim())
        for (part in parts.drop(1)) {
            val split = part.indexOf('=')
            val key = (if (split < 0) part else part.substring(0, split)).trim().lowercase()
            val value = if (split < 0) "" else part.substring(split + 1).trim()
            when (key) {
                "path" -> if (value.startsWith("/")) cookie = cookie.copy(path = value)
                "domain" -> cookie = cookie.copy(domain = value)
                "secure" -> cookie = cookie.copy(secure = true)
                "httponly" -> cookie = cookie.copy(httpOnly = true)
                "expires" -> cookie = cookie.copy(expiresMillis = parseDate(value))
            }
        }
        return cookie
    }

    private fun parseDate(value: String): Long = try {
        val format = SimpleDateFormat("EEE, dd MMM yyyy HH:mm:ss zzz", java.util.Locale.US)
        format.timeZone = java.util.TimeZone.getTimeZone("GMT")
        format.parse(value)?.time ?: 0L
    } catch (_: Exception) {
        0L
    }
}

/** 内容哈希（用于对比档案指纹，避免存明文以外的敏感串）。 */
internal fun sha256Hex(value: String): String {
    val digest = MessageDigest.getInstance("SHA-256").digest(value.toByteArray())
    return digest.joinToString("") { "%02x".format(it) }
}
