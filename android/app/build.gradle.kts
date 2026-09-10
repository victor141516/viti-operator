import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

android {
    namespace = "com.viti.operator"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.viti.operator"
        // Shizuku 13.1.5 sets the practical floor; all Material 3 features degrade gracefully.
        minSdk = 24
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
        vectorDrawables.useSupportLibrary = true
    }

    buildFeatures {
        aidl = true
        compose = true
        buildConfig = true
    }

    compileOptions {
        isCoreLibraryDesugaringEnabled = true
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    packaging {
        resources.excludes += "/META-INF/{AL2.0,LGPL2.1}"
    }

    sourceSets["main"].assets.srcDir(layout.buildDirectory.dir("generated/voiceAssets"))
}

val generateVoiceAssets by tasks.registering(Copy::class) {
    from(rootProject.file("../internal/app/web/voice.bundle.js"))
    into(layout.buildDirectory.dir("generated/voiceAssets"))
}

tasks.named("preBuild").configure { dependsOn(generateVoiceAssets) }

val verifyDebugCarManifest by tasks.registering {
    dependsOn("processDebugManifest")
    doLast {
        val manifest = layout.buildDirectory
            .file("intermediates/merged_manifests/debug/processDebugManifest/AndroidManifest.xml")
            .get()
            .asFile
        check(manifest.isFile) { "Merged debug manifest was not generated" }
        val contents = manifest.readText()
        check("com.google.android.gms.car.application" in contents) { "Android Auto application metadata is missing" }
        check("androidx.car.app.minCarApiLevel" in contents && "android:value=\"5\"" in contents) {
            "The merged manifest must declare minimum Car API level 5"
        }
        check("com.viti.operator.car.VitiCarAppService" in contents) { "The projected CarAppService is missing" }
        check("androidx.car.app.category.CALLING" in contents) { "The calling category is missing" }
        check("android.permission.USE_FULL_SCREEN_INTENT" in contents) {
            "Incoming service calls must declare the full-screen intent capability"
        }
        check("com.viti.operator.IncomingCallActivity" in contents) {
            "The private incoming-call presentation activity is missing"
        }
        val incomingActivity = contents.substringAfter("android:name=\"com.viti.operator.IncomingCallActivity\"")
            .substringBefore("/>")
        check("android:exported=\"false\"" in incomingActivity) {
            "IncomingCallActivity must remain private to explicit app PendingIntents"
        }
        check("android:launchMode=\"singleTask\"" in incomingActivity) {
            "IncomingCallActivity must reuse one task across duplicate call notifications"
        }
        check("android:showWhenLocked=\"true\"" in incomingActivity && "android:turnScreenOn=\"true\"" in incomingActivity) {
            "IncomingCallActivity must be eligible for lock-screen call presentation"
        }
    }
}

tasks.named("check").configure { dependsOn(verifyDebugCarManifest) }

kotlin {
    compilerOptions {
        jvmTarget.set(JvmTarget.JVM_17)
    }
}

dependencies {
    coreLibraryDesugaring("com.android.tools:desugar_jdk_libs:2.1.5")
    // Latest stable Compose BOM that supports the installed Android 16 / API 36 toolchain.
    val composeBom = platform("androidx.compose:compose-bom:2026.06.01")
    implementation(composeBom)
    androidTestImplementation(composeBom)

    implementation("androidx.activity:activity-compose:1.12.3")
    implementation("androidx.core:core-ktx:1.17.0")
    implementation("androidx.core:core-telecom:1.0.1")
    implementation("androidx.car.app:app:1.7.0")
    implementation("androidx.car.app:app-projected:1.7.0")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material3:material3-adaptive-navigation-suite")
    implementation("androidx.compose.material:material-icons-extended")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-tooling-preview")
    debugImplementation("androidx.compose.ui:ui-tooling")

    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.10.0")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.10.0")
    implementation("androidx.lifecycle:lifecycle-viewmodel-ktx:2.10.0")
    implementation("androidx.datastore:datastore-preferences:1.1.7")
    implementation("androidx.webkit:webkit:1.14.0")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")

    implementation("dev.rikka.shizuku:api:13.1.5")
    implementation("dev.rikka.shizuku:provider:13.1.5")

    testImplementation("junit:junit:4.13.2")
    testImplementation("org.jetbrains.kotlinx:kotlinx-coroutines-test:1.10.2")
    androidTestImplementation("androidx.car.app:app-testing:1.7.0")
    androidTestImplementation("androidx.test.ext:junit:1.3.0")
    androidTestImplementation("androidx.test.espresso:espresso-core:3.7.0")
    androidTestImplementation("androidx.compose.ui:ui-test-junit4")
    debugImplementation("androidx.compose.ui:ui-test-manifest")
}
