<template>
  <AppLayout>
    <div class="mx-auto max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
      <div class="mb-5 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">部门成员授权</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            仅能为自己负责范围内的员工分配管理员授权给部门的分组。
          </p>
        </div>
        <button class="btn btn-secondary" :disabled="loading" @click="loadUsers">
          {{ loading ? '刷新中' : '刷新' }}
        </button>
      </div>

      <section class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
        <div class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
              <tr>
                <th class="px-4 py-3">员工</th>
                <th class="px-4 py-3">主部门</th>
                <th class="px-4 py-3">当前部门授权</th>
                <th class="px-4 py-3">可分配分组</th>
                <th class="px-4 py-3 text-right">操作</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="user in users" :key="user.tenant_key + ':' + user.open_id">
                <td class="px-4 py-3">
                  <div class="font-medium text-gray-900 dark:text-white">{{ user.name || user.local_username || user.open_id }}</div>
                  <div class="mt-0.5 text-xs text-gray-500">{{ user.local_email || user.email || user.open_id }}</div>
                </td>
                <td class="px-4 py-3">
                  <div class="text-gray-700 dark:text-gray-200">{{ user.primary_department_name || user.primary_open_department_id }}</div>
                  <div v-if="user.department_open_ids.length > 1" class="mt-1 text-xs text-amber-600 dark:text-amber-300">
                    多部门，仅按主部门授权
                  </div>
                </td>
                <td class="px-4 py-3">{{ formatGroupIds(user.department_manager_group_ids, user.assignable_groups || []) }}</td>
                <td class="px-4 py-3">
                  <div class="flex max-w-lg flex-wrap gap-1.5">
                    <span v-for="group in user.assignable_groups || []" :key="group.id" class="rounded bg-emerald-50 px-2 py-1 text-xs text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-200">
                      {{ group.name }}
                    </span>
                    <span v-if="!user.assignable_groups?.length" class="text-xs text-gray-400">部门暂未授权可分配分组</span>
                  </div>
                </td>
                <td class="px-4 py-3 text-right">
                  <button class="btn btn-sm btn-primary" :disabled="!user.assignable_groups?.length" @click="openEditor(user)">分配</button>
                </td>
              </tr>
              <tr v-if="!loading && users.length === 0">
                <td colspan="5" class="px-4 py-8 text-center text-gray-500">暂无可管理的员工</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </div>

    <div v-if="editor.visible && editor.user" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 px-4">
      <div class="w-full max-w-xl rounded-lg bg-white shadow-xl dark:bg-dark-800">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            分配分组：{{ editor.user.name || editor.user.local_email || editor.user.open_id }}
          </h2>
          <button class="text-gray-400 hover:text-gray-600" @click="closeEditor">×</button>
        </div>
        <div class="max-h-[60vh] overflow-y-auto px-5 py-4">
          <div class="grid gap-2">
            <label v-for="group in editor.user.assignable_groups || []" :key="group.id" class="flex items-center gap-2 rounded border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
              <input v-model="selectedGroupIds" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600" :value="group.id" />
              <span class="min-w-0 flex-1 truncate text-gray-800 dark:text-gray-100">{{ group.name }}</span>
              <span class="text-xs text-gray-400">{{ group.platform }}</span>
            </label>
          </div>
        </div>
        <div class="flex justify-end gap-2 border-t border-gray-200 px-5 py-4 dark:border-dark-700">
          <button class="btn btn-secondary" @click="closeEditor">取消</button>
          <button class="btn btn-primary" :disabled="saving" @click="saveEditor">{{ saving ? '保存中' : '保存' }}</button>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import feishuOrgAPI, { type FeishuOrgGroupBrief, type FeishuOrgUser } from '@/api/admin/feishuOrg'
import { useAppStore } from '@/stores'

const appStore = useAppStore()
const loading = ref(false)
const saving = ref(false)
const users = ref<FeishuOrgUser[]>([])
const selectedGroupIds = ref<number[]>([])
const editor = reactive<{ visible: boolean; user?: FeishuOrgUser }>({ visible: false })

onMounted(() => {
  void loadUsers()
})

async function loadUsers() {
  loading.value = true
  try {
    const result = await feishuOrgAPI.listManagedUsers({ limit: 200 })
    users.value = result.items || []
  } catch (error: any) {
    appStore.showError(error?.message || '加载可管理员工失败')
  } finally {
    loading.value = false
  }
}

function openEditor(user: FeishuOrgUser) {
  editor.visible = true
  editor.user = user
  selectedGroupIds.value = [...user.department_manager_group_ids]
}

function closeEditor() {
  editor.visible = false
  editor.user = undefined
  selectedGroupIds.value = []
}

async function saveEditor() {
  if (!editor.user) return
  saving.value = true
  try {
    await feishuOrgAPI.setManagedUserGroupGrants(editor.user.user_id, {
      group_ids: selectedGroupIds.value,
      reason: 'manager_ui'
    })
    appStore.showSuccess('保存成功')
    closeEditor()
    await loadUsers()
  } catch (error: any) {
    appStore.showError(error?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

function formatGroupIds(ids: number[], groups: FeishuOrgGroupBrief[]) {
  if (!ids || ids.length === 0) return '-'
  return ids.map((id) => groups.find((group) => group.id === id)?.name || `#${id}`).join('、')
}
</script>
