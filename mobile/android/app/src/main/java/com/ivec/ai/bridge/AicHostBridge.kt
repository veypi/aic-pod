package com.ivec.ai.bridge

import android.webkit.JavascriptInterface
import android.webkit.WebView
import org.json.JSONObject

/**
 * 网页与原生壳之间的统一桥：window.aicHost。
 *
 * 协议（能力、方法、事件）见 aic-pod/docs/development/client-bridge.md §七；
 * 与 iOS 壳（AicHostBridge.swift）同一套契约。
 */
class AicHostBridge(private val webView: WebView) {
    /**
     * 页面方法 → 宿主实现。
     * complete(result, errorMessage)：成功给结果，失败给错误文案；两者只应有一个非空。
     */
    var handler: ((String, JSONObject, (JSONObject?, String?) -> Unit) -> Unit)? = null

    /** 页面调用入口（由注入的 JS shim 调用）。 */
    @JavascriptInterface
    fun call(id: String, method: String, paramsJson: String) {
        val params = try { JSONObject(paramsJson) } catch (_: Exception) { JSONObject() }
        val current = handler
        if (current == null) {
            reject(id, "aicHost handler is unavailable")
            return
        }
        current(method, params) { result, error ->
            if (error != null) reject(id, error) else resolve(id, result ?: JSONObject())
        }
    }

    /** 宿主 → 页面事件（如 auth.changed）。 */
    fun emit(event: String, payload: JSONObject) {
        webView.evaluateJavascript(
            "window.__aicHostEmit(${JSONObject.quote(event)}, ${payload.toString()})",
            null
        )
    }

    private fun resolve(id: String, result: JSONObject) {
        webView.evaluateJavascript(
            "window.__aicHostResolve(${JSONObject.quote(id)}, ${result.toString()})",
            null
        )
    }

    private fun reject(id: String, message: String) {
        webView.evaluateJavascript(
            "window.__aicHostReject(${JSONObject.quote(id)}, ${JSONObject.quote(message)})",
            null
        )
    }

    companion object {
        /** 注入页面的桥脚本（页面通过 window.aicHostNative.call 与原生对话）。 */
        const val SCRIPT = """
(function () {
  if (window.aicHost && window.aicHost.version === 1) return;
  var pending = {};
  var listeners = {};
  var seq = 0;

  function post(method, params) {
    return new Promise(function (resolve, reject) {
      var id = 'c' + (++seq);
      pending[id] = { resolve: resolve, reject: reject };
      try {
        window.aicHostNative.call(id, method, JSON.stringify(params || {}));
      } catch (error) {
        delete pending[id];
        reject(new Error(String((error && error.message) || error)));
      }
    });
  }

  window.__aicHostResolve = function (id, result) {
    var entry = pending[id];
    if (!entry) return;
    delete pending[id];
    entry.resolve(result);
  };

  window.__aicHostReject = function (id, message) {
    var entry = pending[id];
    if (!entry) return;
    delete pending[id];
    entry.reject(new Error(message || 'aicHost call failed'));
  };

  window.__aicHostEmit = function (event, payload) {
    var list = listeners[event];
    if (!list) return;
    list.slice().forEach(function (handler) {
      try { handler(payload); } catch (error) { console.error('aicHost handler failed', error); }
    });
  };

  window.aicHost = {
    version: 1,
    platform: 'android',
    capabilities: {
      auth: true,
      accountDelete: true,
      share: false,
      clipboard: false,
      files: false,
      notify: false,
      external: false,
      deeplink: false,
      theme: false,
      locale: false
    },
    call: function (method, params) { return post(method, params); },
    on: function (event, handler) {
      if (typeof handler !== 'function') throw new TypeError('aicHost event handler must be a function');
      var list = listeners[event] || (listeners[event] = []);
      list.push(handler);
      return function () {
        var current = listeners[event] || [];
        var index = current.indexOf(handler);
        if (index >= 0) current.splice(index, 1);
      };
    },
    off: function (event, handler) {
      var current = listeners[event] || [];
      var index = current.indexOf(handler);
      if (index >= 0) current.splice(index, 1);
    }
  };
})();
"""
    }
}
