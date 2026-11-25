import {Card, Switch} from 'antd'
import {SetAutostart, SetAutoRestart, SetCloseToTray} from '../../wailsjs/go/main/App'
import {useLauncher} from '../app/LauncherProvider'

// 开关的唯一真值来自后端快照：调用失败时快照不会变，界面自然回到原状态，
// 所以这里不保存本地镜像状态，也不在渲染时改状态。
function SettingRow({title, hint, checked, disabled, onChange}: {
    title: string
    hint: string
    checked: boolean
    disabled?: boolean
    onChange: (enabled: boolean) => void
}) {
    return <div className="info-row">
        <div className="info-row-main">
            <div className="info-row-title">{title}</div>
            <div className="info-row-hint">{hint}</div>
        </div>
        <div className="info-row-side">
            <Switch aria-label={title} checked={checked} disabled={disabled} onChange={onChange} />
        </div>
    </div>
}

export function SupervisorCard() {
    const {state, action} = useLauncher()
    if (!state) return null
    const supervisor = state.supervisor
    const trayReady = supervisor.tray_ready
    // 有崩溃记录才提示；次数为 0 但留了原因，说明上次崩溃之后已经正常退出过。
    const crashed = supervisor.crash_count > 0 || Boolean(supervisor.last_crash)
    return <Card className="stack-card" title="常驻运行" size="small">
        <SettingRow title="关闭窗口时收进托盘"
            hint={trayReady
                ? '关闭主窗口后启动器留在托盘里，NeoBot 继续运行；从托盘菜单可以真正退出。'
                : '当前环境不支持托盘，关闭窗口将直接退出。'}
            checked={trayReady && supervisor.close_to_tray}
            disabled={!trayReady}
            onChange={enabled => void action(() => SetCloseToTray(enabled))} />
        <SettingRow title="跟随系统登录启动"
            hint="登录系统后自动启动启动器并留在托盘，不会弹出主窗口。"
            checked={supervisor.autostart}
            onChange={enabled => void action(() => SetAutostart(enabled))} />
        <SettingRow title="崩溃后自动重启"
            hint="NeoBot 意外退出后自动重新启动；连续崩溃会逐次延长等待，仍然失败则暂停自动重启。"
            checked={supervisor.auto_restart}
            onChange={enabled => void action(() => SetAutoRestart(enabled))} />
        {crashed && <div className="info-row">
            <div className="info-row-main">
                <span className="state-dot tone-warn"><i aria-hidden="true" />
                    {supervisor.crash_count > 0
                        ? `最近连续异常退出 ${supervisor.crash_count} 次`
                        : '最近有一次异常退出'}
                </span>
                <div className="info-row-hint">{supervisor.last_crash || '请先查看日志排查原因，再启动 NeoBot。'}</div>
            </div>
        </div>}
    </Card>
}
