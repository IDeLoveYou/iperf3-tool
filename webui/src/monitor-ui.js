import { createApp, h, reactive } from 'vue'
import { ElButton, ElIcon, ElLoading, ElMessage, ElMessageBox, ElOption, ElSelect } from 'element-plus'
import { Delete } from '@element-plus/icons-vue'
import 'element-plus/dist/index.css'
import './monitor-ui.css'

const selectorState = reactive({
  tests: [],
  selected: '__new_test__',
  disabled: false
})

// “新增标签”是固定的虚拟选项，不对应后端任务，因此不会显示删除按钮。
const NEW_TEST_VALUE = '__new_test__'

function optionText(test) {
  const stateText = {
    idle: '未运行',
    running: '运行中',
    stopping: '停止中',
    error: '错误'
  }[test.state] || test.state
  const protocol = String(test.protocol || 'udp').toUpperCase()
  // 标签摘要同时显示地址、端口、协议和状态。已有标签的新配置只有在后端
  // 启动成功并提交后才会进入 tests 列表，因此这里不会提前显示候选值。
  return `${test.name} · ${test.host}:${test.port} · ${protocol} · ${stateText}`
}

function emit(name, detail) {
  window.dispatchEvent(new CustomEvent(name, { detail }))
}

async function confirmDelete(event, test) {
  // 阻止删除按钮的鼠标事件冒泡给选项，避免用户点删除时先切换标签。
  event.stopPropagation()
  try {
    await ElMessageBox.confirm(
      `删除“${test.name}”会停止该标签正在运行的监测，并清除它的实时数据和历史记录。`,
      '删除测试标签',
      {
        type: 'warning',
        confirmButtonText: '删除并停止',
        cancelButtonText: '取消',
        confirmButtonClass: 'el-button--danger',
        autofocus: false
      }
    )
    emit('test-delete-request', test.id)
  } catch {
    // 用户取消属于正常操作，不显示错误提示。
  }
}

const app = createApp({
  setup() {
    return () => h(ElSelect, {
      class: 'test-selector',
      modelValue: selectorState.selected,
      disabled: selectorState.disabled,
      placeholder: '暂无监测数据',
      teleported: true,
      'onUpdate:modelValue': value => { selectorState.selected = value },
      onChange: value => emit('test-select-change', value === NEW_TEST_VALUE ? '' : value)
    }, {
      default: () => [
        h(ElOption, {
          key: NEW_TEST_VALUE,
          value: NEW_TEST_VALUE,
          label: '新增标签'
        }, {
          default: () => h('div', { class: 'test-option test-option-new' }, [
            h('span', { class: 'test-option-label' }, '新增标签')
          ])
        }),
        ...selectorState.tests.map(test => h(ElOption, {
          key: test.id,
          value: test.id,
          label: optionText(test)
        }, {
          default: () => h('div', { class: 'test-option' }, [
            h('span', { class: 'test-option-label' }, optionText(test)),
            h(ElButton, {
              class: 'test-option-delete',
              type: 'danger',
              text: true,
              circle: true,
              title: `删除 ${test.name}`,
              'aria-label': `删除 ${test.name}`,
              onMousedown: event => event.stopPropagation(),
              onClick: event => confirmDelete(event, test)
            }, { icon: () => h(ElIcon, null, { default: () => h(Delete) }) })
          ])
        }))
      ]
    })
  }
})

app.mount('#testSelectorApp')

// 配置下拉框使用与标签选择器相同的 Element Plus ElSelect。
// 隐藏 input 只作为现有监测页面的兼容桥接：Go/SSE 页面仍通过原来的
// #protocol 和 #mode 读取值，因此替换视觉组件不会改变配置提交协议。
function mountConfigSelect(containerId, hiddenId, options) {
  const hidden = document.getElementById(hiddenId)
  const state = reactive({
    value: hidden.value,
    disabled: false
  })

  createApp({
    setup() {
      return () => h(ElSelect, {
        class: 'config-select',
        modelValue: state.value,
        disabled: state.disabled,
        teleported: true,
        'onUpdate:modelValue': value => {
          state.value = value
          hidden.value = value
          // 复用页面现有 change 处理：协议变化立即切换仪表盘，
          // 方向变化继续更新新增标签草稿。
          hidden.dispatchEvent(new Event('change', { bubbles: true }))
        }
      }, {
        default: () => options.map(option => h(ElOption, {
          key: option.value,
          value: option.value,
          label: option.label
        }))
      })
    }
  }).mount(`#${containerId}`)

  return {
    setValue(value) {
      state.value = String(value)
      hidden.value = state.value
    },
    setDisabled(disabled) {
      state.disabled = Boolean(disabled)
      hidden.disabled = state.disabled
    }
  }
}

const protocolSelect = mountConfigSelect('protocolSelectApp', 'protocol', [
  { value: 'udp', label: 'UDP' },
  { value: 'tcp', label: 'TCP' }
])
const modeSelect = mountConfigSelect('modeSelectApp', 'mode', [
  { value: 'bidir', label: '双向同时' },
  { value: 'upload', label: '单向上传' },
  { value: 'download', label: '单向下载' }
])

window.configSelects = {
  setValues(protocol, mode) {
    protocolSelect.setValue(protocol)
    modeSelect.setValue(mode)
  },
  setDisabled(disabled) {
    protocolSelect.setDisabled(disabled)
    modeSelect.setDisabled(disabled)
  }
}

// 只暴露最小桥接接口，监测页面仍由 Go/SSE 负责数据和生命周期管理。
window.testSelector = {
  update(tests, selected) {
    selectorState.tests = tests.map(test => ({ ...test }))
    selectorState.selected = selected || NEW_TEST_VALUE
  },
  setDisabled(disabled) {
    selectorState.disabled = Boolean(disabled)
  }
}

let lastAlertMessage = ''

// 直接使用 Element Plus 原生 Message，等价于 Options API 中的
// this.$message(message)。不指定类型、常驻、关闭按钮、位置或动画。
window.testAlert = {
  set(message, force = false) {
    const nextMessage = message || ''
    if (!nextMessage) return
    // 状态轮询可能反复携带同一错误，去重可避免每秒重复弹出。
    // force 用于新的操作请求失败，即使错误文本相同也应再次提示。
    if (!force && nextMessage === lastAlertMessage) return
    lastAlertMessage = nextMessage
    ElMessage(nextMessage)
  }
}

// Element Plus 的 ElMessageBox.alert 等价于 Options API 中的 this.$alert。
// 这里专门用于用户切换到“错误”状态的监测标签时进行确认提示。
window.testErrorAlert = {
  show(message) {
    if (!message) return
    // 监测任务错误必须只显示确认窗。先关闭可能尚未自动消失的
    // 操作 Message，避免用户误以为该任务错误仍然通过 Message 展示。
    ElMessage.closeAll()
    ElMessageBox.alert(message, '监测错误', {
      type: 'error',
      confirmButtonText: '确定'
    }).catch(() => {
      // 用户点击右上角关闭也属于已阅，无需再产生错误。
    })
  }
}

let loadingInstance = null

// 长耗时操作使用全屏 Loading，但采用半透明背景和轻量提示，避免遮罩显得生硬。
window.monitorLoading = {
  start(text) {
    if (loadingInstance) return
    loadingInstance = ElLoading.service({
      fullscreen: true,
      lock: true,
      text: text || '正在处理，请稍候…',
      background: 'rgba(244, 247, 251, 0.62)',
      customClass: 'monitor-loading'
    })
  },
  stop() {
    if (!loadingInstance) return
    loadingInstance.close()
    loadingInstance = null
  }
}
