package i18n

// Notify default-content strings: event labels, field labels, and titles used
// when rendering the platform-default notification (飞书/email/webhook). Localized
// by the notify_config language at send time (background worker, no request locale).
func init() {
	register(map[string]map[string]string{
		// Event labels.
		"构建成功":   {"zh-TW": "建置成功", "en": "Build succeeded", "ja": "ビルド成功", "ko": "빌드 성공", "es": "Compilación exitosa", "fr": "Build réussi", "de": "Build erfolgreich"},
		"构建失败":   {"zh-TW": "建置失敗", "en": "Build failed", "ja": "ビルド失敗", "ko": "빌드 실패", "es": "Compilación fallida", "fr": "Build échoué", "de": "Build fehlgeschlagen"},
		"部署成功":   {"zh-TW": "部署成功", "en": "Deployment succeeded", "ja": "デプロイ成功", "ko": "배포 성공", "es": "Despliegue exitoso", "fr": "Déploiement réussi", "de": "Bereitstellung erfolgreich"},
		"部署失败":   {"zh-TW": "部署失敗", "en": "Deployment failed", "ja": "デプロイ失敗", "ko": "배포 실패", "es": "Despliegue fallido", "fr": "Déploiement échoué", "de": "Bereitstellung fehlgeschlagen"},
		"已回滚":    {"zh-TW": "已回滾", "en": "Rolled back", "ja": "ロールバック済み", "ko": "롤백됨", "es": "Revertido", "fr": "Annulé", "de": "Zurückgerollt"},
		"健康检查失败": {"zh-TW": "健康檢查失敗", "en": "Health check failed", "ja": "ヘルスチェック失敗", "ko": "상태 점검 실패", "es": "Comprobación de estado fallida", "fr": "Échec de la vérification de santé", "de": "Health-Check fehlgeschlagen"},
		"需要审批":   {"zh-TW": "需要審批", "en": "Approval required", "ja": "承認が必要です", "ko": "승인 필요", "es": "Se requiere aprobación", "fr": "Approbation requise", "de": "Genehmigung erforderlich"},
		"异常检测":   {"zh-TW": "異常偵測", "en": "Anomaly detected", "ja": "異常を検出", "ko": "이상 감지", "es": "Anomalía detectada", "fr": "Anomalie détectée", "de": "Anomalie erkannt"},
		"运行参数":   {"zh-TW": "執行參數", "en": "Parameters", "ja": "実行パラメータ", "ko": "실행 매개변수", "es": "Parámetros", "fr": "Paramètres", "de": "Parameter"},

		// Field labels (notification body + feishu card field list).
		"项目":     {"zh-TW": "專案", "en": "Project", "ja": "プロジェクト", "ko": "프로젝트", "es": "Proyecto", "fr": "Projet", "de": "Projekt"},
		"分支":     {"zh-TW": "分支", "en": "Branch", "ja": "ブランチ", "ko": "브랜치", "es": "Rama", "fr": "Branche", "de": "Branch"},
		"提交":     {"zh-TW": "提交", "en": "Commit", "ja": "コミット", "ko": "커밋", "es": "Commit", "fr": "Commit", "de": "Commit"},
		"状态":     {"zh-TW": "狀態", "en": "Status", "ja": "ステータス", "ko": "상태", "es": "Estado", "fr": "Statut", "de": "Status"},
		"耗时":     {"zh-TW": "耗時", "en": "Duration", "ja": "所要時間", "ko": "소요 시간", "es": "Duración", "fr": "Durée", "de": "Dauer"},
		"事件":     {"zh-TW": "事件", "en": "Event", "ja": "イベント", "ko": "이벤트", "es": "Evento", "fr": "Événement", "de": "Ereignis"},
		"来源":     {"zh-TW": "來源", "en": "Source", "ja": "ソース", "ko": "소스", "es": "Origen", "fr": "Source", "de": "Quelle"},
		"类型":     {"zh-TW": "類型", "en": "Type", "ja": "種類", "ko": "유형", "es": "Tipo", "fr": "Type", "de": "Typ"},
		"审批链接":   {"zh-TW": "審批連結", "en": "Approval link", "ja": "承認リンク", "ko": "승인 링크", "es": "Enlace de aprobación", "fr": "Lien d’approbation", "de": "Genehmigungslink"},
		"点击前往审批": {"zh-TW": "點此前往審批", "en": "Click to review", "ja": "クリックして承認", "ko": "클릭하여 승인", "es": "Haga clic para revisar", "fr": "Cliquez pour examiner", "de": "Zum Prüfen klicken"},

		// Titles / misc.
		"Pipewright 通知":      {"zh-TW": "Pipewright 通知", "en": "Pipewright notification", "ja": "Pipewright 通知", "ko": "Pipewright 알림", "es": "Notificación de Pipewright", "fr": "Notification Pipewright", "de": "Pipewright-Benachrichtigung"},
		"Pipewright 流水线消息通知": {"zh-TW": "Pipewright 流水線訊息通知", "en": "Pipewright pipeline notification", "ja": "Pipewright パイプライン通知", "ko": "Pipewright 파이프라인 알림", "es": "Notificación de pipeline de Pipewright", "fr": "Notification de pipeline Pipewright", "de": "Pipewright-Pipeline-Benachrichtigung"},

		// 流水线卡片(企微/钉钉群机器人)字段标签与触发信息文案。
		"流水线":       {"zh-TW": "流水線", "en": "Pipeline", "ja": "パイプライン", "ko": "파이프라인", "es": "Canalización", "fr": "Pipeline", "de": "Pipeline"},
		"流水线环境":     {"zh-TW": "流水線環境", "en": "Pipeline environment", "ja": "パイプライン環境", "ko": "파이프라인 환경", "es": "Entorno de canalización", "fr": "Environnement du pipeline", "de": "Pipeline-Umgebung"},
		"执行人":       {"zh-TW": "執行人", "en": "Executor", "ja": "実行者", "ko": "실행자", "es": "Ejecutor", "fr": "Exécuteur", "de": "Ausführender"},
		"触发信息":      {"zh-TW": "觸發資訊", "en": "Trigger", "ja": "トリガー", "ko": "트리거", "es": "Disparador", "fr": "Déclencheur", "de": "Auslöser"},
		"流水线阶段":     {"zh-TW": "流水線階段", "en": "Pipeline stage", "ja": "パイプラインステージ", "ko": "파이프라인 단계", "es": "Etapa", "fr": "Étape du pipeline", "de": "Pipeline-Phase"},
		"流水线任务":     {"zh-TW": "流水線任務", "en": "Pipeline job", "ja": "パイプライジョブ", "ko": "파이프라인 작업", "es": "Tarea", "fr": "Tâche du pipeline", "de": "Pipeline-Job"},
		"运行状态":      {"zh-TW": "執行狀態", "en": "Run status", "ja": "実行ステータス", "ko": "실행 상태", "es": "Estado de ejecución", "fr": "Statut d’exécution", "de": "Ausführungsstatus"},
		"提交信息":      {"zh-TW": "提交資訊", "en": "Commit message", "ja": "コミットメッセージ", "ko": "커밋 메시지", "es": "Mensaje de commit", "fr": "Message de commit", "de": "Commit-Nachricht"},
		"流水线定时自动触发": {"zh-TW": "流水線定時自動觸發", "en": "Scheduled pipeline trigger", "ja": "パイプラインの定時自動トリガー", "ko": "파이프라인 예약 자동 트리거", "es": "Disparo automático programado", "fr": "Déclenchement planifié automatique", "de": "Geplante Pipeline-Auslösung"},
		"代码推送触发":    {"zh-TW": "程式碼推送觸發", "en": "Code push trigger", "ja": "コードプッシュでトリガー", "ko": "코드 푸시 트리거", "es": "Disparo por push de código", "fr": "Déclenché par un push", "de": "Code-Push-Auslösung"},
		"手动触发":      {"zh-TW": "手動觸發", "en": "Manual trigger", "ja": "手動トリガー", "ko": "수동 트리거", "es": "Disparo manual", "fr": "Déclenchement manuel", "de": "Manuelle Auslösung"},
		"上游运行串联触发":  {"zh-TW": "上游執行串聯觸發", "en": "Triggered by upstream run (chain)", "ja": "上流実行のチェーンでトリガー", "ko": "상위 실행 체인 트리거", "es": "Disparo encadenado por ejecución ascendente", "fr": "Déclenché par enchaînement amont", "de": "Durch vorgelagerten Lauf ausgelöst (Kette)"},
		"(空通知)":     {"zh-TW": "(空通知)", "en": "(empty notification)", "ja": "(空の通知)", "ko": "(빈 알림)", "es": "(notificación vacía)", "fr": "(notification vide)", "de": "(leere Benachrichtigung)"},
		"测试通知已发送":   {"zh-TW": "測試通知已發送", "en": "Test notification sent", "ja": "テスト通知を送信しました", "ko": "테스트 알림을 보냈습니다", "es": "Notificación de prueba enviada", "fr": "Notification de test envoyée", "de": "Testbenachrichtigung gesendet"},
		"通知语言非法:须为受支持的语言代码": {"zh-TW": "通知語言非法:須為受支持的語言代碼", "en": "Invalid notification language: must be a supported language code", "ja": "通知言語が不正です:サポートされている言語コードである必要があります", "ko": "알림 언어가 잘못되었습니다: 지원되는 언어 코드여야 합니다", "es": "Idioma de notificación no válido: debe ser un código de idioma admitido", "fr": "Langue de notification non valide : doit être un code de langue pris en charge", "de": "Ungültige Benachrichtigungssprache: muss ein unterstützter Sprachcode sein"},
	})
}
