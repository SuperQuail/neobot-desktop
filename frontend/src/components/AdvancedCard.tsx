import {useState} from 'react'
import {Alert, App, Button, Card, Dropdown, Space, Tag} from 'antd'
import {OpenFolder, OpenTerminal} from '../../wailsjs/go/main/App'
import {ClipboardSetText} from '../../wailsjs/runtime/runtime'
import {useLauncher} from '../app/LauncherProvider'
import {displayBranch} from '../app/versionLabel'

// 高级终端：点击后直接进入当前版本的 venv。
// 风险提示第一次用弹窗确认，之后每次打开只在卡片里显示一条可关闭的 Alert。
export function AdvancedCard() {
    const {state, action} = useLauncher()
    const {modal, message} = App.useApp()
    const [notice, setNotice] = useState(false)
    if (!state) return null
    const snapshot = state
    const terminal = snapshot.terminal
    const installed = snapshot.installed
    const busy = snapshot.job.state === 'running' || snapshot.starting
    const canOpen = Boolean(terminal?.available) && !busy

    const versions = installed.map(item => ({
        key: item.name,
        label: <span className="version-option">
            <span>{item.branch ? displayBranch(item.branch, item.repository) : item.name}</span>
            {snapshot.running === item.name ? <Tag color="green">运行中</Tag>
                : snapshot.selected === item.name ? <Tag color="blue">下次启动</Tag> : null}
        </span>,
    }))

    function open(version?: string) {
        function run(acknowledged: boolean) {
            return action(async () => {
                await OpenTerminal(version ?? '', acknowledged)
                setNotice(true)
            })
        }
        if (snapshot.terminal_acknowledged) {
            void run(false)
            return
        }
        modal.confirm({
            title: '使用终端前的提示',
            width: 560,
            okText: '我了解，打开终端',
            cancelText: '取消',
            content: <div className="terminal-warning">
                <p>这个终端是普通系统终端，不是沙箱。</p>
                <ul>
                    <li>它拥有当前账户的权限，可以读写 <code>data/.env</code> 和
                        <code>data/.neobot/control.json</code> 中的凭据。</li>
                    <li>修改 Python 环境前请先停止 NeoBot；NeoBot 运行期间不要启动第二个实例。</li>
                    <li>修复或更新可能重置你对 venv 的手动修改；不要手删 <code>versions/</code> 和
                        <code>settings.json</code>。</li>
                </ul>
                <p>确认后不再重复提示。</p>
            </div>,
            onOk: () => run(true),
        })
    }

    async function copyInfo() {
        if (!terminal?.available) return
        const text = [
            `版本：${terminal.label}（${terminal.version}）`,
            `venv：${terminal.venv}`,
            `python：${terminal.python}`,
            `数据目录：${terminal.data}`,
            `uv：${terminal.uv}`,
        ].join('\n')
        try {
            const copied = await ClipboardSetText(text)
            if (copied) void message.success('环境信息已复制')
            else void message.warning('无法复制环境信息')
        } catch {
            void message.warning('无法复制环境信息')
        }
    }

    return <Card className="stack-card" size="small" title="高级">
        {notice && <Alert className="advanced-notice" type="warning" showIcon closable
            title="终端使用提示"
            description={<>
                终端拥有当前账户权限，不是沙箱；<code>data/.env</code> 和
                <code>data/.neobot/control.json</code> 含凭据；修改环境前先停止 NeoBot，
                不要同时启动第二个实例；修复/更新可能重置你对 venv 的改动。
            </>}
            onClose={() => setNotice(false)} />}

        <div className="info-row">
            <div className="info-row-main">
                <div className="info-row-title">打开终端</div>
                <div className="info-row-hint">
                    {terminal?.available
                        ? `进入 ${terminal.label} 的 Python 环境；终端里可使用 python 和 uv。`
                        : terminal?.reason || '请先安装并选择要维护的版本。'}
                </div>
            </div>
            <div className="info-row-side">
                <Space.Compact>
                    <Button type="primary" disabled={!canOpen} onClick={() => open()}>打开终端</Button>
                    <Dropdown disabled={!installed.length || busy}
                        menu={{items: versions, onClick: ({key}) => open(key)}}>
                        <Button aria-label="选择其他版本打开终端">版本</Button>
                    </Dropdown>
                </Space.Compact>
            </div>
        </div>

        <div className="info-row">
            <div className="info-row-main">
                <div className="info-row-title">环境路径</div>
                <div className="info-row-hint tabular" style={{overflowWrap: 'anywhere'}}>
                    {terminal?.available
                        ? <>{terminal.venv}<br />{terminal.python}</>
                        : '安装版本后可在这里查看 venv 路径。'}
                </div>
            </div>
            <div className="info-row-side">
                <Button size="small" disabled={!terminal?.available}
                    onClick={() => void copyInfo()}>复制环境信息</Button>
            </div>
        </div>

        <div className="info-row">
            <div className="info-row-main">
                <div className="info-row-title">目录</div>
                <div className="info-row-hint">打开当前版本的安装目录，或启动器数据根目录。</div>
            </div>
            <div className="info-row-side">
                <Button size="small" disabled={!terminal?.available}
                    onClick={() => void action(() => OpenFolder('version'))}>版本目录</Button>
                <Button size="small" onClick={() => void action(() => OpenFolder('root'))}>启动器目录</Button>
            </div>
        </div>
    </Card>
}
