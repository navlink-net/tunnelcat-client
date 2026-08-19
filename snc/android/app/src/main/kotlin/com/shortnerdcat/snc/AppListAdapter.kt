// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.graphics.drawable.Drawable
import android.view.LayoutInflater
import android.view.ViewGroup
import androidx.recyclerview.widget.RecyclerView
import com.shortnerdcat.snc.databinding.ItemAppBinding

data class AppItem(
    val packageName: String,
    val label: String,
    val icon: Drawable,
    var excluded: Boolean,
)

class AppListAdapter(
    private val allItems: List<AppItem>,
    private val onToggle: (AppItem) -> Unit,
) : RecyclerView.Adapter<AppListAdapter.VH>() {

    private var visibleItems: List<AppItem> = allItems

    inner class VH(private val b: ItemAppBinding) : RecyclerView.ViewHolder(b.root) {
        fun bind(item: AppItem) {
            b.imgAppIcon.setImageDrawable(item.icon)
            b.txtAppName.text = item.label
            b.checkApp.isChecked = item.excluded
            b.root.setOnClickListener {
                item.excluded = !item.excluded
                b.checkApp.isChecked = item.excluded
                onToggle(item)
            }
        }
    }

    override fun onCreateViewHolder(parent: ViewGroup, viewType: Int) =
        VH(ItemAppBinding.inflate(LayoutInflater.from(parent.context), parent, false))

    override fun onBindViewHolder(holder: VH, position: Int) = holder.bind(visibleItems[position])

    override fun getItemCount() = visibleItems.size

    fun filter(query: String) {
        visibleItems = if (query.isBlank()) allItems
        else allItems.filter { it.label.contains(query, ignoreCase = true) }
        notifyDataSetChanged()
    }

    fun selectAll() {
        visibleItems.forEach { item ->
            if (!item.excluded) {
                item.excluded = true
                onToggle(item)
            }
        }
        notifyDataSetChanged()
    }

    fun deselectAll() {
        visibleItems.forEach { item ->
            if (item.excluded) {
                item.excluded = false
                onToggle(item)
            }
        }
        notifyDataSetChanged()
    }
}
