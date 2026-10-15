"""pytest 配置：让 tests 在未 pip install 时也能直接运行（把 embedding 根加入 sys.path）。"""

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
