# 固定的解析程序。待审命令只从 JSON stdin 读取，绝不 Invoke-Expression / 执行 AST。
$ErrorActionPreference = 'Stop'
[Console]::InputEncoding = New-Object System.Text.UTF8Encoding($false)
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$request = [Console]::In.ReadToEnd() | ConvertFrom-Json
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseInput([string]$request.command, [ref]$tokens, [ref]$parseErrors)
$unknowns = New-Object 'System.Collections.Generic.List[string]'
$commands = New-Object 'System.Collections.Generic.List[object]'
$errors = @($parseErrors | ForEach-Object { $_.ErrorId })
$nodes = @($ast.FindAll({ param($node) $true }, $true))
if ($nodes.Count -gt 4096) { $unknowns.Add('node_limit') }
$simpleNodes = @('ScriptBlockAst','NamedBlockAst','PipelineAst','CommandAst','StringConstantExpressionAst','ConstantExpressionAst','CommandParameterAst')
foreach ($node in $nodes) {
    $kind = $node.GetType().Name
    if ($simpleNodes -notcontains $kind) { $unknowns.Add($kind) }
    if ($kind -eq 'NamedBlockAst' -and $node.Statements.Count -gt 1) { $unknowns.Add('multiple_statements') }
    if ($kind -eq 'PipelineAst' -and $node.PipelineElements.Count -gt 1) { $unknowns.Add('pipeline') }
    if ($kind -ne 'CommandAst') { continue }
    if ($node.Redirections.Count -gt 0) { $unknowns.Add('redirection') }
    $arguments = New-Object 'System.Collections.Generic.List[object]'
    for ($i = 1; $i -lt $node.CommandElements.Count; $i++) {
        $element = $node.CommandElements[$i]
        $elementKind = $element.GetType().Name
        $value = ''
        if ($elementKind -eq 'StringConstantExpressionAst' -or $elementKind -eq 'ConstantExpressionAst') {
            $value = [string]$element.Value
        } else {
            $unknowns.Add($elementKind)
        }
        $arguments.Add(@{kind=$elementKind; value=$value})
    }
    $commands.Add(@{name=[string]$node.GetCommandName(); invocation=[string]$node.InvocationOperator; arguments=@($arguments.ToArray())})
}
@{
    commands=@($commands.ToArray())
    unknowns=@($unknowns.ToArray() | Select-Object -Unique)
    syntax_errors=@($errors)
    nodes=$nodes.Count
} | ConvertTo-Json -Depth 8 -Compress
