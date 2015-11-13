import { extend } from 'marbles/utils';
import BackupStore from 'dashboard/stores/backup';
import Dispatcher from 'dashboard/dispatcher';
import FileSize from './filesize';

var backupStoreID = null;

var Backup = React.createClass({
	render: function () {
		return (
			<section className="panel-row full-height" style={{
				maxWidth: 600
			}}>
				<section className="panel full-height" style={{
					padding: '1rem'
				}}>
					<header>
						<h1>Backup cluster</h1>
					</header>

					{this.state.err !== null ? (
						<div className='alert-error'>{this.state.err}</div>
					) : null}

					{this.state.url === null || this.state.err !== null ? (
						<button
							className="btn-green btn-block"
							disabled={this.state.downloadBtnDisabled}
							onClick={this.__handleDownloadBtnClick}>
							{this.state.downloadBtnDisabled ? (
								<span>
									Please Wait...&nbsp;
									{this.state.size !== null ? <FileSize size={this.state.size} /> : null}
								</span>
							) : "Start backup"}
						</button>
					) : (
						<a href={this.state.url} className="btn-green btn-block" download={this.state.filename}>
							Save file (<FileSize size={this.state.size} />)
						</a>
					)}
				</section>
			</section>
		);
	},

	getInitialState: function () {
		return this.__getState();
	},

	componentDidMount: function () {
		BackupStore.addChangeListener(backupStoreID, this.__handleStoreChange);
	},

	componentWillUnmount: function () {
		BackupStore.removeChangeListener(backupStoreID, this.__handleStoreChange);
	},

	__getState: function () {
		var backupState = BackupStore.getState(backupStoreID);
		var state = extend({
			downloadBtnDisabled: backupState.downloading
		}, backupState);
		return state;
	},

	__handleStoreChange: function () {
		this.setState(this.__getState());
	},

	__handleDownloadBtnClick: function (e) {
		e.preventDefault();
		Dispatcher.dispatch({
			name: 'DOWNLOAD_BACKUP'
		});
	}
});

export default Backup;
