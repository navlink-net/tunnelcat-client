import java.time.LocalDateTime
import java.time.format.DateTimeFormatter
import java.time.temporal.ChronoUnit

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// versionCode must strictly increase for Android to treat an install as an
// update — versionName is cosmetic only, Android's installer ignores it.
// Both derived from the same yyyyMMddHHmm build timestamp: versionCode as
// minutes since a fixed epoch (fits in a 32-bit Int for centuries, stays
// monotonic across builds without a manual bump), versionName as the
// human-readable string itself.
val versionTimestamp: String = (project.findProperty("appVersion") as String?)
    ?.takeIf { it.length == 12 && it.all(Char::isDigit) }
    ?: LocalDateTime.now().format(DateTimeFormatter.ofPattern("yyyyMMddHHmm"))

val computedVersionCode: Int = run {
    val fmt = DateTimeFormatter.ofPattern("yyyyMMddHHmm")
    val built = LocalDateTime.parse(versionTimestamp, fmt)
    val epoch = LocalDateTime.of(2024, 1, 1, 0, 0)
    ChronoUnit.MINUTES.between(epoch, built).toInt()
}

android {
    namespace = "com.shortnerdcat.snc"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.shortnerdcat.snc"
        minSdk = 26
        targetSdk = 34
        versionCode = computedVersionCode
        versionName = versionTimestamp

        // arm64-v8a for most real devices; armeabi-v7a for ultra-budget devices
        // that ship a 32-bit-only system image despite a 64-bit-capable chip
        // (e.g. Poco C51 / Redmi A-series on Unisoc T612) -- without it, such
        // devices get "app not compatible with this device" at install since
        // no matching ABI exists in the APK; x86_64 for emulator testing.
        ndk { abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64") }

        externalNativeBuild {
            cmake { cppFlags += "" }
        }
    }

    externalNativeBuild {
        cmake {
            path = file("src/main/cpp/CMakeLists.txt")
            version = "3.22.1"
        }
    }

    signingConfigs {
        create("release") {
            storeFile     = file("D:\\REPO\\tf38key.jks")
            storePassword = System.getenv("SNC_SIGN_PASSWORD") ?: ""
            keyAlias      = "tf38key"
            keyPassword   = System.getenv("SNC_SIGN_PASSWORD") ?: ""
        }
    }

    buildTypes {
        release {
            signingConfig = signingConfigs.getByName("release")
            isMinifyEnabled = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
    }

    buildFeatures { viewBinding = true; buildConfig = true }

    packaging {
        jniLibs { useLegacyPackaging = true }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_1_8
        targetCompatibility = JavaVersion.VERSION_1_8
    }
    kotlinOptions { jvmTarget = "1.8" }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("com.google.android.material:material:1.12.0")
    implementation("androidx.constraintlayout:constraintlayout:2.1.4")
    implementation("androidx.viewpager2:viewpager2:1.1.0")
    implementation("androidx.fragment:fragment-ktx:1.8.3")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.7.3")
    implementation("com.journeyapps:zxing-android-embedded:4.3.0") { isTransitive = false }
    implementation("com.google.zxing:core:3.5.3")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    implementation("androidx.webkit:webkit:1.11.0")
    testImplementation("junit:junit:4.13.2")
}
