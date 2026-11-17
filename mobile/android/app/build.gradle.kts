plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.ivec.ai"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.ivec.ai"
        minSdk = 26
        targetSdk = 34
        versionCode = 1
        versionName = "0.1.0"
    }

    buildFeatures {
        buildConfig = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }
}

// 文案单一真源：client-bridge/l10n.json（仓内共享），构建时复制进 assets。
val syncL10n by tasks.registering(Copy::class) {
    from(rootProject.file("../client-bridge/l10n.json"))
    into(layout.projectDirectory.dir("src/main/assets"))
}

tasks.named("preBuild").configure {
    dependsOn(syncL10n)
}

dependencies {
    implementation("androidx.webkit:webkit:1.11.0")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")
}
