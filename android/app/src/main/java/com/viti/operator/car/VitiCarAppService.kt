package com.viti.operator.car

import android.content.Intent
import android.content.pm.ApplicationInfo
import androidx.car.app.CarAppService
import androidx.car.app.CarContext
import androidx.car.app.Screen
import androidx.car.app.Session
import androidx.car.app.SessionInfo
import androidx.car.app.model.Action
import androidx.car.app.model.Header
import androidx.car.app.model.ItemList
import androidx.car.app.model.ListTemplate
import androidx.car.app.model.MessageTemplate
import androidx.car.app.model.Row
import androidx.car.app.model.Template
import androidx.car.app.validation.HostValidator
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

class VitiCarAppService : CarAppService() {
    override fun createHostValidator(): HostValidator =
        if ((applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE) != 0) {
            HostValidator.ALLOW_ALL_HOSTS_VALIDATOR
        } else {
            HostValidator.Builder(this)
                .addAllowedHosts(androidx.car.app.R.array.hosts_allowlist_sample)
                .build()
        }

    override fun onCreateSession(sessionInfo: SessionInfo): Session = VitiCarSession()
}

private class VitiCarSession : Session() {
    override fun onCreateScreen(intent: Intent): Screen = VitiCarCallsScreen(carContext)
}

internal class VitiCarCallsScreen(
    carContext: CarContext,
    private val controller: CarCallController = CarCallCoordinator(carContext),
) : Screen(carContext) {
    private var stateJob: Job? = null

    init {
        lifecycle.addObserver(object : DefaultLifecycleObserver {
            override fun onCreate(owner: LifecycleOwner) {
                controller.start()
                stateJob = owner.lifecycleScope.launch {
                    controller.state.collect { invalidate() }
                }
            }

            override fun onDestroy(owner: LifecycleOwner) {
                stateJob?.cancel()
                controller.close()
            }
        })
    }

    override fun onGetTemplate(): Template {
        val state = controller.state.value
        return when {
            !state.coreTelecomAvailable -> message(
                "This phone cannot expose Viti calls to Android Auto because Core-Telecom is unavailable.",
            )
            state.notice.isNotBlank() -> message(state.notice, "Back to calls") {
                controller.clearNotice()
            }
            state.call.legToken.isNotBlank() -> message(
                "Android Auto is handling the current Viti call with its system in-call controls.",
            )
            state.directoryError.isNotBlank() -> message(
                "The Viti directory is unavailable: ${state.directoryError}",
                "Retry",
                controller::refresh,
            )
            state.loadingDirectory -> ListTemplate.Builder()
                .setHeader(appHeader())
                .setLoading(true)
                .build()
            state.targets.isEmpty() -> message(
                "No recent calls or synchronized contacts are available. Complete setup in Viti on your phone when safe.",
                "Refresh",
                controller::refresh,
            )
            else -> directory(state.targets)
        }
    }

    private fun directory(targets: List<CarDialTarget>): Template {
        val items = ItemList.Builder()
        targets.forEach { target ->
            items.addItem(
                Row.Builder()
                    .setTitle(target.name)
                    .addText(target.number)
                    .setOnClickListener { controller.call(target) }
                    .build(),
            )
        }
        return ListTemplate.Builder()
            .setHeader(appHeader())
            .setSingleList(items.build())
            .build()
    }

    private fun message(text: String, actionTitle: String? = null, action: (() -> Unit)? = null): Template {
        val builder = MessageTemplate.Builder(text)
            .setHeader(appHeader())
        if (actionTitle != null && action != null) {
            builder.addAction(
                Action.Builder()
                    .setTitle(actionTitle)
                    .setOnClickListener(action)
                    .build(),
            )
        }
        return builder.build()
    }

    private fun appHeader(): Header = Header.Builder()
        .setTitle(TITLE)
        .setStartHeaderAction(Action.APP_ICON)
        .build()

    private companion object {
        const val TITLE = "Viti Operator"
    }
}
