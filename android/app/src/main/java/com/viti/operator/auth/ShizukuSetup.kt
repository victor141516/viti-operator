package com.viti.operator.auth

import android.content.Context
import android.content.ComponentName
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.os.IBinder
import com.viti.operator.BuildConfig
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.CancellableContinuation
import rikka.shizuku.Shizuku
import java.io.IOException

object ShizukuSetup {
    const val REQUEST_CODE = 7331

    fun available(): Boolean = runCatching { Shizuku.pingBinder() }.getOrDefault(false)

    fun authorized(): Boolean = available() &&
        Shizuku.checkSelfPermission() == PackageManager.PERMISSION_GRANTED

    fun requestAuthorization() {
        if (available()) Shizuku.requestPermission(REQUEST_CODE)
    }

    suspend fun grantSimAuthenticationAppOp(context: Context): String = suspendCancellableCoroutine { continuation ->
		withPrivilegedService(context, "sim_permission", continuation) { service ->
			service.grantSimAuthentication(context.packageName)
		}
	}

	suspend fun enableRemoteSimMode(context: Context): String = suspendCancellableCoroutine { continuation ->
		withPrivilegedService(context, "remote_sim_enable", continuation) { service ->
			service.enableRemoteSimMode()
		}
	}

	suspend fun disableRemoteSimMode(context: Context, restoreNetworkMask: String): String = suspendCancellableCoroutine { continuation ->
		withPrivilegedService(context, "remote_sim_disable", continuation) { service ->
			service.disableRemoteSimMode(restoreNetworkMask)
		}
	}

	private fun withPrivilegedService(
		context: Context,
		processSuffix: String,
		continuation: CancellableContinuation<String>,
		operation: (IPrivilegedService) -> String,
	) {
        if (!authorized()) throw IOException("Authorize Viti Operator in Shizuku first")
        val args = Shizuku.UserServiceArgs(ComponentName(context, PrivilegedService::class.java))
            .daemon(false)
			.processNameSuffix(processSuffix)
            .debuggable(BuildConfig.DEBUG)
            .version(BuildConfig.VERSION_CODE)
        lateinit var connection: ServiceConnection
        connection = object : ServiceConnection {
            override fun onServiceConnected(name: ComponentName, binder: IBinder) {
                val result = runCatching {
					operation(IPrivilegedService.Stub.asInterface(binder))
                }
                runCatching { Shizuku.unbindUserService(args, connection, true) }
                continuation.resumeWith(result)
            }

            override fun onServiceDisconnected(name: ComponentName) {
                if (continuation.isActive) {
                    continuation.resumeWith(Result.failure(IOException("The Shizuku service disconnected")))
                }
            }
        }
        continuation.invokeOnCancellation {
            runCatching { Shizuku.unbindUserService(args, connection, true) }
        }
        runCatching { Shizuku.bindUserService(args, connection) }
            .onFailure { continuation.resumeWith(Result.failure(it)) }
    }
}
