package com.ivec.ai

import android.annotation.SuppressLint
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.view.View
import android.webkit.CookieManager
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import androidx.webkit.WebViewCompat
import com.ivec.ai.auth.AuthManager
import com.ivec.ai.auth.CredentialStore
import com.ivec.ai.bridge.AicHostBridge
import com.ivec.ai.util.L10n
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import org.json.JSONObject
import java.net.URL

/**
 * 壳入口：原生登录页 ↔ 平台工作区（WebView）。
 *
 * 登录态由原生认证管理器持有；网页只通过 window.aicHost 说话。
 */
class MainActivity : android.app.Activity() {
    private val scope = CoroutineScope(Dispatchers.Main + SupervisorJob())

    private lateinit var auth: AuthManager
    private lateinit var bridge: AicHostBridge

    private var webView: WebView? = null
    private lateinit var loginPanel: View
    private lateinit var accountInput: EditText
    private lateinit var passwordInput: EditText
    private lateinit var serverInput: EditText
    private lateinit var errorView: TextView

    /** 当前平台地址（登录页可改）。 */
    private var server: String = DEFAULT_SERVER

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        L10n.load(this)
        setContentView(R.layout.activity_main)

        loginPanel = findViewById(R.id.login_panel)
        accountInput = findViewById(R.id.account)
        passwordInput = findViewById(R.id.password)
        serverInput = findViewById(R.id.server)
        errorView = findViewById(R.id.error)

        server = getSharedPreferences(PREFS, MODE_PRIVATE).getString(KEY_SERVER, DEFAULT_SERVER) ?: DEFAULT_SERVER
        serverInput.setText(server)

        auth = AuthManager(server, CredentialStore(this))
        auth.onStateChanged = { state ->
            runOnUiThread { onStateChanged(state.signedIn) }
        }
        auth.onCookiesChanged = { cookies ->
            runOnUiThread { syncCookies(cookies) }
        }
        auth.restore()

        findViewById<Button>(R.id.sign_in).setOnClickListener { signIn() }
        findViewById<TextView>(R.id.app_name).text = L10n.text("app.name")
        findViewById<TextView>(R.id.subtitle).text = L10n.text("login.subtitle")
        findViewById<TextView>(R.id.server_label).text = L10n.text("login.server")
        findViewById<TextView>(R.id.account_label).text = L10n.text("login.account")
        findViewById<TextView>(R.id.password_label).text = L10n.text("login.password_value")
        findViewById<Button>(R.id.sign_in).text = L10n.text("login.submit")
    }

    override fun onDestroy() {
        scope.coroutineContext.cancel()
        webView?.destroy()
        super.onDestroy()
    }

    // MARK: - 登录

    private fun signIn() {
        val target = serverInput.text.toString().trim().ifEmpty { DEFAULT_SERVER }
        val account = accountInput.text.toString().trim()
        val password = passwordInput.text.toString()
        if (account.isEmpty() || password.isEmpty()) {
            errorView.text = L10n.text("login.required")
            return
        }
        errorView.text = ""
        if (target != server) {
            server = target
            getSharedPreferences(PREFS, MODE_PRIVATE).edit().putString(KEY_SERVER, target).apply()
            auth.switchServer(target)
            rebuildWebView()
        }
        scope.launch {
            try {
                auth.login(account, password)
                passwordInput.setText("")
                showWorkspace()
            } catch (error: Exception) {
                errorView.text = error.message ?: L10n.text("settings.operation_failed")
            }
        }
    }

    // MARK: - 工作区

    @SuppressLint("SetJavaScriptEnabled")
    private fun rebuildWebView() {
        webView?.let { existing ->
            (existing.parent as? android.view.ViewGroup)?.removeView(existing)
            existing.destroy()
        }
        val created = WebView(this)
        created.settings.javaScriptEnabled = true
        created.settings.domStorageEnabled = true
        created.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView?, request: WebResourceRequest?): Boolean {
                val target = request?.url ?: return false
                if (target.host == platformHost()) return false
                // 站外链接交给系统浏览器：壳内只留平台 origin（原生桥只注入受信任 origin）。
                runCatching { startActivity(Intent(Intent.ACTION_VIEW, target)) }
                return true
            }
        }
        bridge = AicHostBridge(created)
        bridge.handler = { method, params, complete -> handleBridge(method, params, complete) }
        created.addJavascriptInterface(bridge, "aicHostNative")
        WebViewCompat.addDocumentStartJavaScript(created, AicHostBridge.SCRIPT, setOf(platformOrigin()))
        val container = findViewById<android.view.ViewGroup>(R.id.web_container)
        container.addView(created)
        webView = created
        syncCookies(emptyList())
        created.loadUrl(workspaceUrl())
    }

    private fun handleBridge(
        method: String,
        params: JSONObject,
        complete: (JSONObject?, String?) -> Unit
    ) {
        scope.launch {
            val result = try {
                when (method) {
                    "app_info" -> JSONObject()
                        .put("version", BuildConfig.VERSION_NAME)
                        .put("platform", "android")
                        .put("build", BuildConfig.VERSION_CODE.toString())
                    "auth_state" -> auth.state.toJson()
                    "auth_refresh" -> auth.refresh().toJson()
                    "auth_sign_out" -> {
                        auth.signOut()
                        JSONObject().put("ok", true)
                    }
                    "auth_open_login" -> {
                        showLogin()
                        JSONObject().put("ok", true)
                    }
                    "account_delete" -> {
                        auth.deleteAccount()
                        showLogin()
                        JSONObject().put("ok", true)
                    }
                    "page_error" -> JSONObject().put("ok", true)
                    else -> {
                        complete(null, "unsupported aicHost method: $method")
                        return@launch
                    }
                }
            } catch (error: Exception) {
                complete(null, error.message ?: "aicHost call failed")
                return@launch
            }
            complete(result, null)
        }
    }

    private fun onStateChanged(signedIn: Boolean) {
        if (signedIn) showWorkspace() else showLogin()
    }

    private fun showLogin() {
        loginPanel.visibility = View.VISIBLE
        webView?.visibility = View.GONE
    }

    private fun showWorkspace() {
        loginPanel.visibility = View.GONE
        if (webView == null) rebuildWebView()
        webView?.visibility = View.VISIBLE
    }

    /** 把壳持有的会话 cookie 同步给 WebView。 */
    private fun syncCookies(cookies: List<Pair<String, String>>) {
        val manager = CookieManager.getInstance()
        val url = platformOrigin()
        cookies.forEach { (name, value) ->
            manager.setCookie(url, "$name=$value; Path=/; Secure")
        }
        manager.flush()
    }

    private fun platformHost(): String? = try {
        URL(server).host
    } catch (_: Exception) {
        null
    }

    private fun platformOrigin(): String = try {
        val url = URL(server)
        "${url.protocol}//${url.host}"
    } catch (_: Exception) {
        DEFAULT_SERVER
    }

    private fun workspaceUrl(): String = server.trimEnd('/') + WORKSPACE_PATH

    companion object {
        private const val PREFS = "ivec.app"
        private const val KEY_SERVER = "server"
        private const val DEFAULT_SERVER = "https://ivec-ai.com"
        private const val WORKSPACE_PATH = "/u"
    }
}
