// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.os.Bundle
import android.text.Editable
import android.text.TextWatcher
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.Toast
import androidx.fragment.app.Fragment
import androidx.lifecycle.lifecycleScope
import androidx.recyclerview.widget.LinearLayoutManager
import com.shortnerdcat.snc.databinding.FragmentAppsBinding
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class AppsFragment : Fragment() {

    private var _binding: FragmentAppsBinding? = null
    private val binding get() = _binding!!
    private var adapter: AppListAdapter? = null

    override fun onCreateView(inflater: LayoutInflater, container: ViewGroup?, savedInstanceState: Bundle?): View {
        _binding = FragmentAppsBinding.inflate(inflater, container, false)
        return binding.root
    }

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        super.onViewCreated(view, savedInstanceState)
        binding.rvApps.layoutManager = LinearLayoutManager(requireContext())

        binding.etFilter.addTextChangedListener(object : TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, start: Int, count: Int, after: Int) = Unit
            override fun onTextChanged(s: CharSequence?, start: Int, before: Int, count: Int) = Unit
            override fun afterTextChanged(s: Editable?) {
                adapter?.filter(s?.toString() ?: "")
            }
        })

        binding.btnSelectAll.setOnClickListener {
            adapter?.selectAll()
            notifyReconnectIfNeeded()
        }

        binding.btnDeselectAll.setOnClickListener {
            adapter?.deselectAll()
            notifyReconnectIfNeeded()
        }

        loadApps()
    }

    override fun onDestroyView() {
        super.onDestroyView()
        _binding = null
        adapter = null
    }

    private fun loadApps() {
        val ctx = requireContext()
        binding.progressApps.visibility = View.VISIBLE

        lifecycleScope.launch {
            val items = withContext(Dispatchers.IO) {
                try {
                    val pm = ctx.packageManager
                    val excluded = ExcludedApps.load(ctx)
                    val ownPkg = ctx.packageName
                    // GET_META_DATA is not used; skip it to avoid TransactionTooLargeException
                    // on devices with many apps.
                    pm.getInstalledApplications(0)
                        .filter { info ->
                            if (info.packageName == ownPkg) return@filter false
                            val isSystem = (info.flags and ApplicationInfo.FLAG_SYSTEM) != 0
                            val isUpdated = (info.flags and ApplicationInfo.FLAG_UPDATED_SYSTEM_APP) != 0
                            !isSystem || isUpdated
                        }
                        .mapNotNull { info ->
                            try {
                                AppItem(
                                    packageName = info.packageName,
                                    label = pm.getApplicationLabel(info).toString(),
                                    icon = pm.getApplicationIcon(info),
                                    excluded = info.packageName in excluded,
                                )
                            } catch (_: Exception) { null }
                        }
                        .sortedBy { it.label.lowercase() }
                } catch (_: Exception) {
                    emptyList()
                }
            }

            binding.progressApps.visibility = View.GONE
            binding.rvApps.visibility = View.VISIBLE
            adapter = AppListAdapter(items) { item ->
                val current = ExcludedApps.load(ctx).toMutableSet()
                if (item.excluded) current.add(item.packageName) else current.remove(item.packageName)
                ExcludedApps.save(ctx, current)
                notifyReconnectIfNeeded()
            }
            binding.rvApps.adapter = adapter
        }
    }

    private fun notifyReconnectIfNeeded() {
        if (SNCVpnService.isRunning) {
            Toast.makeText(requireContext(), getString(R.string.apps_reconnect_hint), Toast.LENGTH_SHORT).show()
        }
    }
}
