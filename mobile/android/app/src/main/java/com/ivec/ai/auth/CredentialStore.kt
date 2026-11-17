package com.ivec.ai.auth

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import org.json.JSONObject
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * 登录档案的加密存储：密钥在 AndroidKeyStore，偏好文件里只有密文。
 * 按服务器隔离；登出只清当前账户的凭据，其它账户的档案保留。
 */
class CredentialStore(context: Context) {
    private val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    /** 读档案；不存在或解密失败返回 null。 */
    fun load(host: String): Archive? {
        val raw = prefs.getString(key(host), null) ?: return null
        return try {
            val payload = Base64.decode(raw, Base64.DEFAULT)
            if (payload.size <= IV_SIZE) return null
            val cipher = Cipher.getInstance(TRANSFORMATION)
            cipher.init(Cipher.DECRYPT_MODE, secretKey(), GCMParameterSpec(TAG_BITS, payload, 0, IV_SIZE))
            val plain = cipher.doFinal(payload, IV_SIZE, payload.size - IV_SIZE)
            Archive.fromJson(JSONObject(String(plain)))
        } catch (_: Exception) {
            null
        }
    }

    /** 写档案（原子替换）。 */
    fun save(host: String, archive: Archive) {
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(Cipher.ENCRYPT_MODE, secretKey())
        val cipherText = cipher.doFinal(archive.toJson().toString().toByteArray())
        val payload = cipher.iv + cipherText
        prefs.edit().putString(key(host), Base64.encodeToString(payload, Base64.DEFAULT)).apply()
    }

    /** 清掉某台服务器的档案。 */
    fun clear(host: String) {
        prefs.edit().remove(key(host)).apply()
    }

    private fun key(host: String) = "archive.$host"

    private fun secretKey(): SecretKey {
        val keyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
        (keyStore.getEntry(KEY_ALIAS, null) as? KeyStore.SecretKeyEntry)?.let { return it.secretKey }
        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, ANDROID_KEYSTORE)
        generator.init(
            KeyGenParameterSpec.Builder(
                KEY_ALIAS,
                KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT
            )
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .build()
        )
        return generator.generateKey()
    }

    /** 档案：当前账户 + 各账户凭据。 */
    data class Account(val credentials: AuthCredentials) {
        fun toJson(): JSONObject = JSONObject().put("credentials", credentials.toJson())

        companion object {
            fun fromJson(json: JSONObject): Account = Account(AuthCredentials.fromJson(json.getJSONObject("credentials")))
        }
    }

    data class Archive(val currentUid: String, val accounts: Map<String, Account>) {
        fun toJson(): JSONObject {
            val accountsJson = JSONObject()
            accounts.forEach { (uid, account) -> accountsJson.put(uid, account.toJson()) }
            return JSONObject().put("currentUid", currentUid).put("accounts", accountsJson)
        }

        companion object {
            fun fromJson(json: JSONObject): Archive {
                val accountsJson = json.getJSONObject("accounts")
                val accounts = mutableMapOf<String, Account>()
                accountsJson.keys().forEach { uid -> accounts[uid] = Account.fromJson(accountsJson.getJSONObject(uid)) }
                return Archive(currentUid = json.optString("currentUid"), accounts = accounts)
            }
        }
    }

    companion object {
        private const val PREFS = "ivec.auth"
        private const val ANDROID_KEYSTORE = "AndroidKeyStore"
        private const val KEY_ALIAS = "ivec.auth.key"
        private const val TRANSFORMATION = "AES/GCM/NoPadding"
        private const val IV_SIZE = 12
        private const val TAG_BITS = 128
    }
}
