package com.ivec.ai.auth

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/** 壳侧认证错误。 */
class AuthException(message: String, val status: Int = 0) : Exception(message)

/**
 * 原生认证管理器：壳是登录态的唯一所有者。
 *
 * 持有 access / refresh token（Keystore 加密存储），统一执行登录、刷新、登出与注销；
 * WebView 里的 cookie 只是网页请求需要的镜像。刷新只有一个执行者（互斥 + 过期判断）。
 */
class AuthManager(
    private var server: String,
    private val store: CredentialStore
) {
    /** 登录态变化回调（桥发 auth.changed）。 */
    var onStateChanged: ((AuthState) -> Unit)? = null
    /** 会话 cookie 变化回调（同步给 WebView）。 */
    var onCookiesChanged: ((List<Pair<String, String>>) -> Unit)? = null

    var state: AuthState = AuthState(false, null, 0)
        private set

    private var credentials: AuthCredentials? = null
    private val refreshMutex = Mutex()

    val isSignedIn: Boolean get() = state.signedIn

    /** 切换平台地址：档案按服务器隔离。 */
    fun switchServer(next: String) {
        if (next == server) return
        server = next
        credentials = null
        publish(AuthState(false, null, 0))
        publishCookies(emptyList())
        restore()
    }

    /** 冷启动恢复：从加密档案读回凭据。 */
    fun restore() {
        val host = host()
        val archive = store.load(host) ?: return
        val account = archive.accounts[archive.currentUid] ?: return
        credentials = account.credentials
        publish(AuthState(true, account.credentials.user, account.credentials.expiresAtMillis))
        publishCookies(cookiesFor(account.credentials))
    }

    suspend fun login(username: String, password: String) {
        authenticate("/vb/api/auth/login", JSONObject().put("username", username).put("password", password))
    }

    suspend fun loginCode(type: String, target: String, code: String) {
        authenticate("/vb/api/auth/login/code", JSONObject().put("type", type).put("target", target).put("code", code))
    }

    suspend fun register(body: JSONObject) {
        authenticate("/vb/api/auth/register", body)
    }

    /** 统一刷新：并发调用共享同一次请求（互斥 + 过期判断）。 */
    suspend fun refresh(): AuthState = refreshMutex.withLock {
        val current = credentials ?: throw AuthException("未登录", 401)
        if (current.expiresAtMillis > System.currentTimeMillis() + 30_000) return@withLock state
        val result = request(
            path = "/vb/api/auth/refresh",
            method = "POST",
            body = JSONObject(),
            headers = mapOf("Cookie" to "${current.refreshCookieName}=${current.refreshToken}")
        )
        val next = credentialsFrom(result)
        if (next.user.id != current.user.id) {
            clearLocal()
            throw AuthException("刷新后账户发生变化，已停止连接", 401)
        }
        val archive = store.load(host()) ?: CredentialStore.Archive("", emptyMap())
        store.save(host(), archive.copy(currentUid = next.user.id, accounts = archive.accounts + (next.user.id to CredentialStore.Account(next))))
        credentials = next
        publish(AuthState(true, next.user, next.expiresAtMillis))
        publishCookies(cookiesFor(next))
        state
    }

    /** 供后续 Host 接入使用的最新 access token。 */
    suspend fun accessToken(): String {
        val current = credentials ?: throw AuthException("未登录", 401)
        if (current.expiresAtMillis <= System.currentTimeMillis() + 30_000) refresh()
        return credentials?.accessToken ?: current.accessToken
    }

    /** 登出：先吊销服务端会话，再清本机凭据与 cookie。 */
    suspend fun signOut() {
        credentials?.let { current ->
            try {
                request(
                    path = "/vb/api/auth/logout",
                    method = "POST",
                    body = JSONObject(),
                    headers = mapOf("Authorization" to "Bearer ${current.accessToken}")
                )
            } catch (_: Exception) {
                // 服务端不可达时也要清本机登录态。
            }
        }
        clearLocal()
    }

    /** 注销账号：服务端删除账号，本机凭据一并清掉。 */
    suspend fun deleteAccount() {
        val current = credentials ?: throw AuthException("未登录", 401)
        request(
            path = "/vb/api/auth/account",
            method = "DELETE",
            body = null,
            headers = mapOf("Authorization" to "Bearer ${current.accessToken}")
        )
        clearLocal()
    }

    // MARK: - 内部

    private suspend fun authenticate(path: String, body: JSONObject) {
        val result = request(path, "POST", body, emptyMap())
        val next = credentialsFrom(result)
        val archive = store.load(host()) ?: CredentialStore.Archive("", emptyMap())
        val accounts = archive.accounts.toMutableMap()
        if (archive.currentUid.isNotEmpty() && archive.currentUid != next.user.id) {
            accounts.remove(archive.currentUid)
        }
        accounts[next.user.id] = CredentialStore.Account(next)
        store.save(host(), CredentialStore.Archive(next.user.id, accounts))
        credentials = next
        publish(AuthState(true, next.user, next.expiresAtMillis))
        publishCookies(cookiesFor(next))
    }

    private fun clearLocal() {
        val archive = store.load(host())
        if (archive != null && archive.currentUid.isNotEmpty()) {
            val accounts = archive.accounts.toMutableMap()
            accounts.remove(archive.currentUid)
            store.save(host(), CredentialStore.Archive("", accounts))
        }
        credentials = null
        publish(AuthState(false, null, 0))
        publishCookies(emptyList())
    }

    private fun publish(next: AuthState) {
        state = next
        onStateChanged?.invoke(next)
    }

    private fun publishCookies(cookies: List<Pair<String, String>>) {
        onCookiesChanged?.invoke(cookies)
    }

    /** 凭据对应的 cookie（access + refresh）。 */
    private fun cookiesFor(credentials: AuthCredentials): List<Pair<String, String>> = listOf(
        credentials.accessCookieName to credentials.accessToken,
        credentials.refreshCookieName to credentials.refreshToken
    )

    private fun host(): String = URL(server).host

    /** 从登录 / 刷新响应里取凭据：只认 HttpOnly 的 access + refresh cookie。 */
    private fun credentialsFrom(result: HttpResult): AuthCredentials {
        val access = result.cookies.firstOrNull { it.name.lowercase().endsWith("access") }
            ?: result.cookies.firstOrNull { JWT.type(it.value) == "access" }
        val refresh = result.cookies.firstOrNull { it.name.lowercase().endsWith("refresh") }
            ?: result.cookies.firstOrNull { JWT.type(it.value) == "refresh" }
        if (access == null || refresh == null || !access.httpOnly || !refresh.httpOnly ||
            access.value.isEmpty() || refresh.value.isEmpty()
        ) {
            throw AuthException("登录响应缺少 HttpOnly 会话凭证")
        }
        val payload = JWT.payload(access.value)
        if (payload?.optString("type") != "access" || payload.optString("mod") != "inherit") {
            throw AuthException("登录响应不是动态 Web access token")
        }
        val uid = JWT.uid(access.value) ?: throw AuthException("登录响应不是动态 Web access token")
        val refreshPayload = JWT.payload(refresh.value)
        if (refreshPayload?.optString("type") != "refresh" || JWT.uid(refresh.value) != uid) {
            throw AuthException("登录响应的 refresh token 与用户身份不一致")
        }
        val body = try { JSONObject(result.body) } catch (_: Exception) { JSONObject() }
        val info = body.optJSONObject("user") ?: JSONObject()
        if (info.has("id") && info.getString("id") != uid) {
            throw AuthException("登录用户与访问凭证不一致")
        }
        val user = AuthUser(
            id = uid,
            username = info.optString("username").ifEmpty { null },
            nickname = info.optString("nickname").ifEmpty { null }
        )
        val expiresIn = body.optLong("expires_in", 900L) * 1000L
        return AuthCredentials(
            accessToken = access.value,
            refreshToken = refresh.value,
            accessCookieName = access.name,
            refreshCookieName = refresh.name,
            user = user,
            expiresAtMillis = JWT.expiresAt(access.value, System.currentTimeMillis() + expiresIn)
        )
    }

    private suspend fun request(
        path: String,
        method: String,
        body: JSONObject?,
        headers: Map<String, String>
    ): HttpResult = withContext(Dispatchers.IO) {
        val url = URL(server.trimEnd('/') + "/" + path.trimStart('/'))
        val connection = url.openConnection() as HttpURLConnection
        try {
            connection.requestMethod = method
            connection.instanceFollowRedirects = false
            connection.connectTimeout = 20_000
            connection.readTimeout = 20_000
            connection.setRequestProperty("Accept", "application/json")
            headers.forEach { (key, value) -> connection.setRequestProperty(key, value) }
            if (body != null) {
                connection.doOutput = true
                connection.setRequestProperty("Content-Type", "application/json")
                connection.outputStream.use { it.write(body.toString().toByteArray()) }
            }
            val status = connection.responseCode
            val stream = if (status in 200..299) connection.inputStream else connection.errorStream
            val text = stream?.bufferedReader()?.use { it.readText() } ?: ""
            val cookies = connection.headerFields
                .filterKeys { it != null && it.equals("Set-Cookie", ignoreCase = true) }
                .flatMap { it.value }
                .flatMap { SetCookie.split(it) }
                .mapNotNull { SetCookie.parse(it) }
            if (status !in 200..299) {
                val message = try {
                    JSONObject(text).optString("message")
                } catch (_: Exception) {
                    ""
                }
                throw AuthException(if (message.isEmpty()) "服务返回 $status" else message, status)
            }
            HttpResult(status, text, cookies)
        } finally {
            connection.disconnect()
        }
    }

    private data class HttpResult(val status: Int, val body: String, val cookies: List<ParsedCookie>)
}
